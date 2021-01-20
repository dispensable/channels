package irc

import (
	"io"
	"net"
	"time"

	"github.com/sirupsen/logrus"
)

// connection corresponds to some end-point that has connected to the IRC
// server.
type connection interface {
	send(message)

	// loop reads messages from the connection and passes them to the handler.
	loop()

	// kill stops the execution of the go routine running Loop.
	kill()
}

type connectionImpl struct {
	config    *Config
	conn      net.Conn
	handler   handler
	inbox     chan message
	inject    chan message // Allows the connection to inject messages.
	gotPong   chan struct{}
	killPing  chan struct{}
	killRead  chan struct{}
	killWrite chan struct{}
}

// newConnection creates a new connection with the given network connection and
// handler.
func newConnection(config *Config, conn net.Conn, handler handler) connection {
	return &connectionImpl{
		config:    config,
		conn:      conn,
		handler:   handler,
		inbox:     make(chan message),
		inject:    make(chan message, 1),
		gotPong:   make(chan struct{}, 1),
		killPing:  make(chan struct{}, 1),
		killRead:  make(chan struct{}, 1),
		killWrite: make(chan struct{}, 1),
	}
}

func (c *connectionImpl) send(msg message) {
	c.inbox <- msg
}

func (c *connectionImpl) loop() {
	go c.writeLoop()
	go c.readLoop()
	c.pingLoop()
}

func (c *connectionImpl) kill() {
	go func() {
		c.killRead <- struct{}{}
		c.killWrite <- struct{}{}
		c.killPing <- struct{}{}
	}()
}

func (c *connectionImpl) readLoop() {
	var msg message
	parser := newMessageParser(c.conn)
	readTimeout := time.Duration(c.config.PongMaxLatency) * time.Second

	didQuit := false
	alive, hasMore := true, true
	for hasMore && alive {
		select {
		case <-c.killRead:
			alive = false
		case msg = <-c.inject:
			logrus.Debugf("inject: %+v", msg)
			didQuit = didQuit || msg.command == cmdQuit.command
			c.handler = c.handler.handle(c, msg)
		default:
			c.conn.SetReadDeadline(time.Now().Add(readTimeout))
			msg, hasMore = parser()
			if msg.command == "" {
				continue
			}

			logrus.Debugf("recv: %+v", msg)

			// Notify the ping thread that we got a ping.
			if msg.command == cmdPong.command {
				c.gotPong <- struct{}{}
			}

			didQuit = didQuit || msg.command == cmdQuit.command
			logrus.Debugf("SEND MSG %v to handler ..", msg)
			c.handler = c.handler.handle(c, msg)
		}
	}

	c.conn.Close()

	// If there was never a QUIT message then this is a premature termination and
	// a quit message should be faked.
	if !didQuit {
		logrus.Debugln("Injecting QUIT for prematurely disconnected client.")
		c.handler = c.handler.handle(c, cmdQuit.withTrailing("QUITing"))
	}

	logrus.Debugln("Closing read loop.")
}

func (c *connectionImpl) writeLoop() {
	alive := true
	for alive {
		select {
		case <-c.killWrite:
			alive = false
		case msg := <-c.inbox:
			logrus.Debugf("send: %+v", msg)

			line, ok := msg.toString()
			if !ok {
				break
			}

			_, err := io.WriteString(c.conn, line)
			if err != nil {
				logrus.Warnf("Error encountered sending message to client: %v", err)
				// break
			}
		}
	}

	logrus.Debugln("Closing write loop.")
	c.conn.Close()
}

func (c *connectionImpl) pingLoop() {
	pingDuration := time.Duration(c.config.PingFrequency) * time.Second
	pongDuration := time.Duration(c.config.PongMaxLatency) * time.Second

	alive := true
	var pongTimer <-chan time.Time
	for alive {
		select {
		case <-c.killPing:
			alive = false
		case <-c.gotPong:
			pongTimer = nil
		case <-pongTimer:
			c.inject <- cmdQuit.withTrailing("Timed out")
		case <-time.After(pingDuration):
			c.inbox <- cmdPing.withTrailing(c.config.Name)
			pongTimer = time.After(pongDuration)
		}
	}
	logrus.Debugln("Closing ping loop.")

}

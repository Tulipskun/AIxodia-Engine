package wsock

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ServerConn is the inbound half of RFC 6455: a connection the engine accepted
// itself. It is the mirror of Client — nothing it sends is masked — and it
// carries the same message-level API, so a handler written against Client's
// ReadMessage/WriteMessage runs unchanged on a socket the engine owns.
type ServerConn struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
	// liveness bounds how long ReadMessage waits for any frame. A phone that
	// vanishes without a close frame leaves the reader parked until TCP
	// keepalive gives up, which is minutes; the peer pings on a shorter
	// interval, so a silent socket is treated as gone.
	liveness time.Duration
}

// NewServerConn wraps a connection handed over by Accept.
func NewServerConn(conn net.Conn, br *bufio.Reader) *ServerConn {
	return &ServerConn{conn: conn, br: br}
}

// SetLiveness makes ReadMessage give up when no frame of any kind arrives
// within d. Zero waits forever.
func (c *ServerConn) SetLiveness(d time.Duration) { c.liveness = d }

// ReadMessage returns the next text or binary frame, answering pings and
// surfacing a close frame as io.EOF.
func (c *ServerConn) ReadMessage() (byte, []byte, error) {
	for {
		if c.liveness > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.liveness)); err != nil {
				return 0, nil, err
			}
		}
		op, payload, err := ReadFrame(c.br)
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			if err := c.WriteMessage(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.WriteMessage(OpClose, nil)
			return 0, nil, io.EOF
		case OpText, OpBinary:
			return op, payload, nil
		default:
			return 0, nil, fmt.Errorf("wsock: unknown opcode %d", op)
		}
	}
}

// WriteMessage sends one unmasked text or binary frame, as a server must.
func (c *ServerConn) WriteMessage(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteFrame(c.conn, op, payload)
}

// Close closes the underlying connection.
func (c *ServerConn) Close() error { return c.conn.Close() }

// SetReadDeadline bounds the next ReadMessage directly.
func (c *ServerConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

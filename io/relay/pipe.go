package relay

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"aixodia-engine/io/wsock"
)

// pipeConn is a net.Conn whose two directions are fed by the relay: Feed hands
// the handler the bytes of one frame, and whatever the handler writes is
// handed to sink frame by frame. It lets the existing socket handlers — the
// phone hub and the terminal loop, both of which speak frames to a net.Conn —
// run unchanged behind one relay socket.
type pipeConn struct {
	mu     sync.Mutex
	cond   *sync.Cond
	in     bytes.Buffer
	wbuf   []byte
	closed bool
	sink   func(op byte, payload []byte) error
}

func newPipeConn(sink func(op byte, payload []byte) error) *pipeConn {
	c := &pipeConn{sink: sink}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *pipeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	for c.in.Len() == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.in.Len() == 0 {
		c.mu.Unlock()
		return 0, io.EOF
	}
	n, _ := c.in.Read(p)
	c.mu.Unlock()
	return n, nil
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.wbuf = append(c.wbuf, p...)
	var out []frame
	for {
		n, op, payload, ok := splitFrame(c.wbuf)
		if !ok {
			break
		}
		c.wbuf = c.wbuf[n:]
		out = append(out, frame{op: op, payload: payload})
	}
	c.mu.Unlock()
	for _, f := range out {
		if c.sink == nil {
			continue
		}
		if err := c.sink(f.op, f.payload); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

type frame struct {
	op      byte
	payload []byte
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "relay" }
func (pipeAddr) String() string  { return "relay" }

func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *pipeConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

// Feed delivers one frame to the reading side. Frames are encoded unmasked,
// which is what a server-side reader expects.
func (c *pipeConn) Feed(op byte, payload []byte) error {
	var buf bytes.Buffer
	if err := wsock.WriteFrame(&buf, op, payload); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return io.EOF
	}
	c.in.Write(buf.Bytes())
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

// splitFrame reads one whole frame off the front of b. It reports false when b
// does not hold a complete frame yet.
func splitFrame(b []byte) (int, byte, []byte, bool) {
	if len(b) < 2 {
		return 0, 0, nil, false
	}
	op := b[0] & 0x0f
	masked := b[1]&0x80 != 0
	ln := int(b[1] & 0x7f)
	off := 2
	switch ln {
	case 126:
		if len(b) < 4 {
			return 0, 0, nil, false
		}
		ln = int(binary.BigEndian.Uint16(b[2:4]))
		off = 4
	case 127:
		if len(b) < 10 {
			return 0, 0, nil, false
		}
		ln = int(binary.BigEndian.Uint64(b[2:10]))
		off = 10
	}
	if ln < 0 || ln > wsock.MaxFrame {
		return 0, 0, nil, false
	}
	var mask [4]byte
	if masked {
		if len(b) < off+4 {
			return 0, 0, nil, false
		}
		copy(mask[:], b[off:off+4])
		off += 4
	}
	if len(b) < off+ln {
		return 0, 0, nil, false
	}
	payload := make([]byte, ln)
	copy(payload, b[off:off+ln])
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return off + ln, op, payload, true
}

package wsock

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is the outbound half of RFC 6455: the engine dials the relay with it.
// A client masks every frame it sends and may not receive masked frames, which
// is the mirror of the server half in Accept/ReadFrame/WriteFrame.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
	// liveness bounds how long ReadMessage waits for any frame at all. Zero
	// means wait forever. A peer that has gone away without a close frame —
	// a dropped phone network, a relay edge that forgot the socket — leaves a
	// reader parked on read until TCP keepalive eventually gives up, which on
	// Linux is minutes. The engine pings on a shorter interval, so a live
	// socket always answers within liveness; a silent one times out.
	liveness time.Duration
}

// Dial opens a WebSocket to a ws:// or wss:// URL. The extra headers are sent
// on the handshake, which is where the bearer token goes.
func Dial(ctx context.Context, rawurl string, header http.Header) (*Client, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	var addr string
	var secure bool
	switch u.Scheme {
	case "ws":
		addr = u.Host
	case "wss":
		addr = u.Host
		secure = true
	default:
		return nil, fmt.Errorf("wsock: scheme %q is not ws or wss", u.Scheme)
	}
	if !strings.Contains(addr, ":") {
		if secure {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	if secure {
		host, _, _ := net.SplitHostPort(addr)
		td := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host}}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, u.Host, key)
	// The caller's headers may not override the handshake ones, but a caller
	// that sets Host or the key by accident would break the exchange silently,
	// so those are skipped rather than merged.
	for name, values := range header {
		switch http.CanonicalHeaderKey(name) {
		case "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Host":
			continue
		}
		for _, v := range values {
			fmt.Fprintf(&req, "%s: %s\r\n", name, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("wsock: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	sum := sha1.Sum([]byte(key + GUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		conn.Close()
		return nil, errors.New("wsock: bad Sec-WebSocket-Accept")
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: conn, br: br}, nil
}

// ReadMessage returns the next text or binary message, answering pings and
// surfacing a close frame as io.EOF so callers have one way to notice the end.
func (c *Client) ReadMessage() (byte, []byte, error) {
	var op byte
	var payload []byte
	for {
		// Refresh the deadline on every frame, not once per message: a pong
		// answers the ping and says the peer is still there, and it must count.
		// Without this an idle connection that only exchanges pings would time
		// out while it is perfectly healthy.
		if c.liveness > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.liveness)); err != nil {
				return 0, nil, err
			}
		}
		fin, frameOp, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch frameOp {
		case OpPing:
			if err := c.WriteMessage(OpPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.WriteMessage(OpClose, nil)
			return 0, nil, io.EOF
		case OpText, OpBinary:
			if op != 0 && op != frameOp {
				return 0, nil, errors.New("wsock: opcode changed mid-message")
			}
			op = frameOp
			payload = append(payload, data...)
		case 0:
			// continuation
			payload = append(payload, data...)
		default:
			return 0, nil, fmt.Errorf("wsock: unknown opcode %d", frameOp)
		}
		if len(payload) > MaxFrame {
			return 0, nil, errors.New("wsock: message too large")
		}
		if fin {
			return op, payload, nil
		}
	}
}

// WriteMessage sends one masked text or binary frame. A client must mask, so
// the server half's WriteFrame cannot be reused here.
func (c *Client) WriteMessage(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeMasked(c.conn, op, payload)
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// SetReadDeadline bounds the next ReadMessage, for callers that would rather
// time out than block on a silent peer.
func (c *Client) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetLiveness makes ReadMessage give up when no frame of any kind arrives
// within d. Every frame — data, ping or pong — resets the clock, so a peer that
// is only answering pings still counts as alive. Zero disables the bound.
func (c *Client) SetLiveness(d time.Duration) { c.liveness = d }

func (c *Client) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin := h[0]&0x80 != 0
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > MaxFrame {
		return false, 0, nil, errors.New("wsock: frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

func writeMasked(w io.Writer, op byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// Package wsock is the small RFC 6455 implementation the engine's WebSocket
// endpoints share: the handshake and the frame reader and writer. Stdlib only
// (framing is implemented here). It deliberately carries no policy — the token
// check, the meaning of a frame, and what happens on disconnect stay with the
// endpoint that owns them — because the phone socket and the terminal socket
// want different things from the same wire format.
package wsock

import (
	"bufio"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// GUID is the magic value RFC 6455 mixes with the client's key.
const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// MaxFrame caps one frame: a client that asks for a bigger one is cut off
// rather than allowed to make the daemon allocate whatever it asks for.
const MaxFrame = 1 << 20

// Opcodes, as RFC 6455 numbers them.
const (
	OpText   = 0x1
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA
)

// ErrNotUpgrade means the request was not a WebSocket upgrade. Nothing has
// been written to the response when it is returned, so the caller can still
// answer with a plain error.
var ErrNotUpgrade = errors.New("websocket upgrade required")

// ErrNoHijack means the response writer cannot hand over its connection, which
// a plain net/http recorder cannot. Nothing has been written when it is
// returned, so the caller can still answer with a plain error.
var ErrNoHijack = errors.New("hijack unsupported")

// BearerOK reports whether the handshake carries the engine token. An empty
// configured token never matches, so an engine started without CF_TOKEN cannot
// end up serving everyone who knocks.
func BearerOK(r *http.Request, token string) bool {
	auth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return token != "" && subtle.ConstantTimeCompare([]byte(auth), []byte(token)) == 1
}

// IsUpgrade reports whether the request carries the whole RFC 6455 handshake.
func IsUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
		r.Header.Get("Sec-WebSocket-Version") == "13" &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

// Accept completes the handshake: it takes the connection away from net/http,
// writes the 101, and hands back the connection and its buffered reader. The
// caller owns the connection from here and closes it. ErrNotUpgrade means the
// request was not an upgrade and nothing was written; any other error means
// the response may already be half-written, so the caller should not write.
func Accept(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	if !IsUpgrade(r) {
		return nil, nil, ErrNotUpgrade
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, ErrNoHijack
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + GUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw.Reader, nil
}

// ReadFrame reads one frame and returns its opcode and unmasked payload. What
// an opcode means is the caller's business; frame size is capped here.
func ReadFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > MaxFrame {
		return 0, nil, errors.New("frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

// WriteFrame writes one unmasked frame, which is what a server sends; only
// clients mask.
func WriteFrame(w io.Writer, op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

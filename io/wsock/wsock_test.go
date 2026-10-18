package wsock

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerOK(t *testing.T) {
	cases := []struct {
		name   string
		header string
		token  string
		want   bool
	}{
		{"matching token", "Bearer good", "good", true},
		{"wrong token", "Bearer bad", "good", false},
		{"no header", "", "good", false},
		{"engine has no token", "Bearer good", "", false},
		{"extra text after the token", "Bearer good extra", "good", false},
		// The scheme is optional here because /ws never required it: the
		// header is compared after TrimPrefix, so a bare token matched before
		// this package existed and must keep matching.
		{"bare token, no scheme", "good", "good", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/terminal", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			if got := BearerOK(r, c.token); got != c.want {
				t.Fatalf("BearerOK(%q, %q) = %v, want %v", c.header, c.token, got, c.want)
			}
		})
	}
}

func TestAcceptRejectsPlainRequest(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/terminal", nil)
	conn, br, err := Accept(w, r)
	if !errors.Is(err, ErrNotUpgrade) {
		t.Fatalf("Accept error = %v, want ErrNotUpgrade", err)
	}
	if conn != nil || br != nil {
		t.Fatalf("Accept returned conn=%v reader=%v, want nils", conn, br)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("Accept wrote a response: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestAcceptRejectsAWriterThatCannotHijack(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/terminal", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	conn, br, err := Accept(w, r)
	if !errors.Is(err, ErrNoHijack) {
		t.Fatalf("Accept error = %v, want ErrNoHijack", err)
	}
	if conn != nil || br != nil {
		t.Fatalf("Accept returned conn=%v reader=%v, want nils", conn, br)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("Accept wrote a response: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestAcceptWritesTheHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := Accept(w, r)
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		defer conn.Close()
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// The RFC 6455 example key, whose accept value is fixed by the spec.
	req := "GET /terminal HTTP/1.1\r\n" +
		"Host: engine\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response (%q so far): %v", head.String(), err)
		}
		if line == "\r\n" {
			break
		}
		head.WriteString(line)
	}
	got := head.String()
	if !strings.HasPrefix(got, "HTTP/1.1 101 ") {
		t.Fatalf("status line = %q, want 101", strings.SplitN(got, "\r\n", 2)[0])
	}
	if !strings.Contains(got, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") {
		t.Fatalf("handshake response lacks the RFC example accept value:\n%s", got)
	}
}

func TestReadWriteFrameRoundTrip(t *testing.T) {
	for _, n := range []int{5, 200, 70000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		var buf bytes.Buffer
		if err := WriteFrame(&buf, OpBinary, payload); err != nil {
			t.Fatalf("WriteFrame(len=%d): %v", n, err)
		}
		op, got, err := ReadFrame(bufio.NewReader(&buf))
		if err != nil {
			t.Fatalf("ReadFrame(len=%d): %v", n, err)
		}
		if op != OpBinary {
			t.Fatalf("op = %#x, want %#x", op, OpBinary)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload round trip differs at len=%d", n)
		}
	}
}

func TestReadFrameUnmasksClientFrames(t *testing.T) {
	payload := []byte("hello masked")
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	var buf bytes.Buffer
	buf.WriteByte(0x80 | OpBinary)
	buf.WriteByte(0x80 | byte(len(payload)))
	buf.Write(mask[:])
	for i, b := range payload {
		buf.WriteByte(b ^ mask[i%4])
	}
	op, got, err := ReadFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if op != OpBinary || !bytes.Equal(got, payload) {
		t.Fatalf("op=%#x payload=%q, want %#x %q", op, got, OpBinary, payload)
	}
}

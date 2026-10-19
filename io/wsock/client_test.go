package wsock

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func wsURL(t *testing.T, s *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestClientEchoesWithMaskedFramesAndAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !BearerOK(r, "secret") {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		conn, br, err := Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			op, payload, err := ReadFrame(br)
			if err != nil {
				return
			}
			if op == OpClose {
				_ = WriteFrame(conn, OpClose, nil)
				return
			}
			if err := WriteFrame(conn, op, payload); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	c, err := Dial(ctx, wsURL(t, srv), h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.WriteMessage(OpText, []byte(`{"ch":"chat"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != OpText || string(payload) != `{"ch":"chat"}` {
		t.Fatalf("echo = %d %q", op, payload)
	}

	blob := make([]byte, 4096)
	for i := range blob {
		blob[i] = byte(i)
	}
	if err := c.WriteMessage(OpBinary, blob); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	op, payload, err = c.ReadMessage()
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if op != OpBinary || len(payload) != len(blob) {
		t.Fatalf("binary echo = %d len=%d", op, len(payload))
	}
	for i := range blob {
		if payload[i] != blob[i] {
			t.Fatalf("binary byte %d = %d", i, payload[i])
		}
	}
}

func TestClientAnswersServerPing(t *testing.T) {
	gotPong := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, br, err := Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = WriteFrame(conn, OpPing, []byte("hi"))
		for {
			op, payload, err := ReadFrame(br)
			if err != nil {
				return
			}
			if op == OpPong {
				gotPong <- string(payload)
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(t, srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	done := make(chan struct{})
	go func() {
		_, _, _ = c.ReadMessage()
		close(done)
	}()
	select {
	case p := <-gotPong:
		if p != "hi" {
			t.Fatalf("pong payload = %q", p)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("client did not answer the ping")
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("read did not finish")
	}
}

func TestClientCloseIsEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := Accept(w, r)
		if err != nil {
			return
		}
		_ = WriteFrame(conn, OpClose, nil)
		conn.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(t, srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, _, err := c.ReadMessage(); err != io.EOF {
		t.Fatalf("close = %v, want io.EOF", err)
	}
}

// A peer that accepts the handshake and then says nothing (no data, no ping,
// no close) must not park the reader forever: liveness turns the silence into
// an error the relay can reconnect on.
func TestClientLivenessTimesOutSilentPeer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(t, srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetLiveness(200 * time.Millisecond)

	start := time.Now()
	_, _, err = c.ReadMessage()
	if err == nil {
		t.Fatal("ReadMessage returned nil, want a timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("liveness took %v, want well under 3s", time.Since(start))
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a net.Error timeout", err)
	}
}

// An idle but healthy peer is kept alive by its pings: each pong refreshes the
// liveness clock, so a socket that never carries data is not torn down.
func TestClientLivenessSurvivesPongs(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, br, err := Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			t := time.NewTicker(50 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if err := WriteFrame(conn, OpPing, nil); err != nil {
						return
					}
				}
			}
		}()
		for {
			op, payload, err := ReadFrame(br)
			if err != nil {
				return
			}
			if op == OpPong {
				continue
			}
			if op == OpText {
				_ = WriteFrame(conn, OpText, payload)
				return
			}
		}
	}))
	defer srv.Close()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(t, srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetLiveness(300 * time.Millisecond)

	// Wait past several pings before sending anything: the pongs must have kept
	// the reader alive, so this round trip succeeds instead of timing out.
	time.Sleep(900 * time.Millisecond)
	if err := c.WriteMessage(OpText, []byte("still here")); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read after pings: %v", err)
	}
	if op != OpText || string(payload) != "still here" {
		t.Fatalf("echo = %d %q", op, payload)
	}
}

func TestClientRejectsUnauthorizedHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token rejected", http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, wsURL(t, srv), nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dial error = %v, want a 401", err)
	}
}

// The client half must mask: a frame it writes has the mask bit set on the
// wire. ReadFrame on the server side already unmasks, so this asserts the bit
// itself rather than a round trip.
func TestClientMasksFrames(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := Accept(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 2)
		if _, err := io.ReadFull(bufio.NewReader(conn), buf); err != nil {
			return
		}
		got <- buf
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(t, srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.WriteMessage(OpText, []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case b := <-got:
		if b[1]&0x80 == 0 {
			t.Fatalf("mask bit not set: %08b", b[1])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server never saw the frame")
	}
}

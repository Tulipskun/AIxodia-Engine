package relay

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aixodia-engine/io/mobile"
	"aixodia-engine/io/terminal"
	"aixodia-engine/io/wsock"
)

type wireFrame struct {
	op      byte
	payload []byte
}

type wire struct {
	conn   net.Conn
	frames chan wireFrame
	hello  map[string]any
}

// fakeRelay accepts the engine's dial like the Worker does and hands the test
// the raw socket, so every assertion is about what actually crossed the wire.
type fakeRelay struct {
	srv  *httptest.Server
	seen chan *wire
}

func newFakeRelay(t *testing.T, token string) *fakeRelay {
	t.Helper()
	fr := &fakeRelay{seen: make(chan *wire, 4)}
	fr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wsock.BearerOK(r, token) {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		conn, br, err := wsock.Accept(w, r)
		if err != nil {
			return
		}
		wc := &wire{conn: conn, frames: make(chan wireFrame, 16)}
		op, payload, err := wsock.ReadFrame(br)
		if err != nil || op != wsock.OpText {
			conn.Close()
			return
		}
		_ = json.Unmarshal(payload, &wc.hello)
		go func() {
			for {
				op, payload, err := wsock.ReadFrame(br)
				if err != nil {
					close(wc.frames)
					return
				}
				wc.frames <- wireFrame{op: op, payload: payload}
			}
		}()
		fr.seen <- wc
	}))
	t.Cleanup(fr.srv.Close)
	return fr
}

func (fr *fakeRelay) url() string {
	return "ws" + strings.TrimPrefix(fr.srv.URL, "http") + "/room/test"
}

func (fr *fakeRelay) accept(t *testing.T) *wire {
	t.Helper()
	select {
	case wc := <-fr.seen:
		return wc
	case <-time.After(5 * time.Second):
		t.Fatal("engine never dialled the relay")
		return nil
	}
}

func (w *wire) sendText(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := wsock.WriteFrame(w.conn, wsock.OpText, data); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func (w *wire) sendBinary(t *testing.T, payload []byte) {
	t.Helper()
	if err := wsock.WriteFrame(w.conn, wsock.OpBinary, payload); err != nil {
		t.Fatalf("send binary: %v", err)
	}
}

// waitFor reads until pred accepts a frame. The engine may interleave ping
// frames, so callers see everything and decide.
func (w *wire) waitFor(t *testing.T, what string, pred func(f wireFrame) bool) wireFrame {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case f, ok := <-w.frames:
			if !ok {
				t.Fatalf("relay socket closed while waiting for %s", what)
			}
			if pred(f) {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (w *wire) waitJSON(t *testing.T, what string, pred func(m map[string]any) bool) map[string]any {
	t.Helper()
	f := w.waitFor(t, what, func(f wireFrame) bool {
		if f.op != wsock.OpText {
			return false
		}
		var m map[string]any
		if json.Unmarshal(f.payload, &m) != nil {
			return false
		}
		return pred(m)
	})
	var m map[string]any
	if err := json.Unmarshal(f.payload, &m); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return m
}

func start(t *testing.T, cfg Config) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return cancel, done
}

func TestEngineAnnouncesItselfAndRunsRPC(t *testing.T) {
	fr := newFakeRelay(t, "tok")

	api := http.NewServeMux()
	api.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		_, _ = w.Write([]byte(`{"pong":"` + r.URL.RawQuery + `"}`))
	})
	api.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
	api.HandleFunc("/who", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	})

	var connected, disconnected int32
	start(t, Config{
		URL: fr.url(), Token: "tok", Started: 1234, Version: "test",
		Hub: mobile.NewHub(), API: api,
		OnConnect:    func() { atomic.AddInt32(&connected, 1) },
		OnDisconnect: func() { atomic.AddInt32(&disconnected, 1) },
		Logf:         func(string, ...any) {},
	})
	wc := fr.accept(t)

	if wc.hello["role"] != "engine" {
		t.Fatalf("hello role = %v", wc.hello["role"])
	}
	if wc.hello["started"] != float64(1234) || wc.hello["version"] != "test" {
		t.Fatalf("hello = %+v", wc.hello)
	}

	wc.sendText(t, map[string]any{"ch": "rpc", "rid": "r1", "m": "GET", "p": "/ping?x=1"})
	m := wc.waitJSON(t, "rpc reply", func(m map[string]any) bool { return m["rid"] == "r1" })
	if m["s"] != float64(200) {
		t.Fatalf("status = %v", m["s"])
	}
	body, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["b"]))
	if string(body) != `{"pong":"x=1"}` {
		t.Fatalf("body = %q", body)
	}
	hdr, _ := m["h"].(map[string]any)
	if hdr["X-Test"] != "1" {
		t.Fatalf("headers = %v", m["h"])
	}

	wc.sendText(t, map[string]any{"ch": "rpc", "rid": "r2", "m": "POST", "p": "/echo", "b": base64.StdEncoding.EncodeToString([]byte("hel")), "more": true})
	wc.sendText(t, map[string]any{"ch": "rpc", "rid": "r2", "m": "POST", "p": "/echo", "b": base64.StdEncoding.EncodeToString([]byte("lo"))})
	m = wc.waitJSON(t, "chunked reply", func(m map[string]any) bool { return m["rid"] == "r2" })
	body, _ = base64.StdEncoding.DecodeString(fmt.Sprint(m["b"]))
	if string(body) != "hello" {
		t.Fatalf("chunked body = %q", body)
	}

	wc.sendText(t, map[string]any{"ch": "rpc", "rid": "r3", "m": "GET", "p": "/who"})
	m = wc.waitJSON(t, "who reply", func(m map[string]any) bool { return m["rid"] == "r3" })
	body, _ = base64.StdEncoding.DecodeString(fmt.Sprint(m["b"]))
	if string(body) != "Bearer tok" {
		t.Fatalf("api saw authorization %q", body)
	}

	if atomic.LoadInt32(&connected) != 1 {
		t.Fatalf("connected = %d", connected)
	}
}

func TestEngineCarriesChatFramesUnchanged(t *testing.T) {
	fr := newFakeRelay(t, "tok")
	start(t, Config{
		URL: fr.url(), Token: "tok",
		Hub: mobile.NewHub(), Chat: func(context.Context, string, string, []mobile.Attachment, func(string), func(mobile.ToolEvent)) (mobile.ChatResult, error) {
			return mobile.ChatResult{}, nil
		},
		Logf: func(string, ...any) {},
	})
	wc := fr.accept(t)

	wc.sendText(t, map[string]any{"ch": "chat", "type": "hello", "session_id": "s1"})
	m := wc.waitJSON(t, "hello ack", func(m map[string]any) bool { return m["kind"] == "ack" })
	if m["ch"] != "chat" || m["session_id"] != "s1" {
		t.Fatalf("ack = %+v", m)
	}

	wc.sendText(t, map[string]any{"ch": "chat", "type": "cancel", "session_id": "s1"})
	m = wc.waitJSON(t, "cancel done", func(m map[string]any) bool { return m["kind"] == "done" })
	if m["stage"] != "already_done" || m["ch"] != "chat" {
		t.Fatalf("done = %+v", m)
	}

	// bye is the app closing: same cancel, without dropping the relay socket.
	wc.sendText(t, map[string]any{"ch": "ctl", "act": "bye"})
	m = wc.waitJSON(t, "bye done", func(m map[string]any) bool { return m["kind"] == "done" })
	if m["stage"] != "already_done" {
		t.Fatalf("bye done = %+v", m)
	}
}

func TestEngineCarriesTerminalStreams(t *testing.T) {
	fr := newFakeRelay(t, "tok")
	term := terminal.NewServer("tok")
	term.Shell = "/bin/sh"
	term.PingEvery = time.Minute
	start(t, Config{
		URL: fr.url(), Token: "tok",
		Hub:  mobile.NewHub(),
		Term: term,
		Logf: func(string, ...any) {},
	})
	wc := fr.accept(t)

	wc.sendText(t, map[string]any{"ch": "ctl", "act": "open", "id": "t1", "sid": 7, "cols": 80, "rows": 24})
	ready := wc.waitJSON(t, "terminal ready", func(m map[string]any) bool {
		return m["ch"] == "term" && m["sid"] == float64(7)
	})
	p, _ := ready["p"].(map[string]any)
	if p["type"] != "ready" || ready["id"] != "t1" {
		t.Fatalf("ready = %+v", ready)
	}

	marker := "RELAY_MARKER"
	payload := append([]byte{ChTerm, 0, 7}, []byte("echo "+marker+"\r")...)
	wc.sendBinary(t, payload)

	var out strings.Builder
	deadline := time.After(8 * time.Second)
	for !strings.Contains(out.String(), marker) {
		select {
		case f, ok := <-wc.frames:
			if !ok {
				t.Fatalf("socket closed; terminal output so far %q", out.String())
			}
			if f.op != wsock.OpBinary || len(f.payload) < 3 || f.payload[0] != ChTerm {
				continue
			}
			if binary.BigEndian.Uint16(f.payload[1:3]) == 7 {
				out.Write(f.payload[3:])
			}
		case <-deadline:
			t.Fatalf("terminal never echoed %q; output %q", marker, out.String())
		}
	}

	wc.sendText(t, map[string]any{"ch": "ctl", "act": "close", "sid": 7})
	// After close, terminal output for sid 7 must not reach the wire.
	before := out.Len()
	wc.sendBinary(t, append([]byte{ChTerm, 0, 7}, []byte("echo LATE\r")...))
	stop := time.After(500 * time.Millisecond)
	for {
		select {
		case f, ok := <-wc.frames:
			if !ok {
				return
			}
			if f.op == wsock.OpBinary && len(f.payload) >= 3 && f.payload[0] == ChTerm &&
				binary.BigEndian.Uint16(f.payload[1:3]) == 7 {
				out.Write(f.payload[3:])
			}
		case <-stop:
			if strings.Contains(out.String()[before:], "LATE") {
				t.Fatalf("closed stream still forwarded output")
			}
			return
		}
	}
}

func TestEngineExitsWhenSuperseded(t *testing.T) {
	fr := newFakeRelay(t, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{URL: fr.url(), Token: "tok", Hub: mobile.NewHub(), Logf: func(string, ...any) {}})
	}()
	wc := fr.accept(t)
	wc.sendText(t, map[string]any{"ctl": "superseded"})
	select {
	case err := <-done:
		if err != ErrSuperseded {
			t.Fatalf("Run returned %v, want ErrSuperseded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestEngineReconnectsWhenTheRelayDrops(t *testing.T) {
	fr := newFakeRelay(t, "tok")
	var connects int32
	start(t, Config{
		URL: fr.url(), Token: "tok", Hub: mobile.NewHub(),
		OnConnect: func() { atomic.AddInt32(&connects, 1) },
		Logf:      func(string, ...any) {},
	})
	first := fr.accept(t)
	first.conn.Close()
	second := fr.accept(t)
	if second.hello["role"] != "engine" {
		t.Fatalf("second hello = %+v", second.hello)
	}
	if atomic.LoadInt32(&connects) < 2 {
		t.Fatalf("connects = %d", connects)
	}
}

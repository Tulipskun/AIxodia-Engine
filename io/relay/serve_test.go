package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aixodia-engine/io/mobile"
	"aixodia-engine/io/wsock"
)

// directServer stands an engine up the way the quick tunnel will reach it: the
// relay handler on a plain httptest server, with the same hub and API the real
// mux wires in.
func directServer(t *testing.T, token string, api http.Handler) (*httptest.Server, *mobile.Hub) {
	t.Helper()
	hub := mobile.NewHub()
	chat := mobile.ChatFunc(func(ctx context.Context, sessionID, text string, retry bool,
		attachments []mobile.Attachment, onDelta func(string), onTool func(mobile.ToolEvent)) (mobile.ChatResult, error) {
		return mobile.ChatResult{}, nil
	})
	h := Handler(Config{
		Token:    token,
		APIToken: token,
		Hub:      hub,
		Chat:     chat,
		API:      api,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, hub
}

func dialDirect(t *testing.T, srv *httptest.Server, token string) *wsock.Client {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := wsock.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readText reads one text frame, failing the test when it takes too long.
func readText(t *testing.T, c *wsock.Client) string {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != wsock.OpText {
		t.Fatalf("opcode = %d, want text", op)
	}
	return string(payload)
}

// atomicString is a one-value mailbox between the test and the API handler,
// which serves on another goroutine.
type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) set(s string) { a.mu.Lock(); a.v = s; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func TestDirectSocketRejectsAMissingToken(t *testing.T) {
	srv, _ := directServer(t, "secret", http.NotFoundHandler())
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// The phone's client only leaves CONNECTING when it hears presence, and it
// sends no hello before that. Presence must therefore be the first frame.
func TestDirectSocketAnnouncesPresenceFirst(t *testing.T) {
	srv, _ := directServer(t, "secret", http.NotFoundHandler())
	c := dialDirect(t, srv, "secret")
	var got map[string]any
	if err := json.Unmarshal([]byte(readText(t, c)), &got); err != nil {
		t.Fatal(err)
	}
	if got["ctl"] != "presence" || got["engine"] != true {
		t.Fatalf("first frame = %v, want presence/engine", got)
	}
}

// The client sends {"role":"phone"} on open. The relay used to swallow it; a
// direct socket reaches the hub, which would read the empty body as a blank
// message and answer with an error the user sees.
func TestDirectSocketDropsTheRoleNote(t *testing.T) {
	srv, _ := directServer(t, "secret", http.NotFoundHandler())
	c := dialDirect(t, srv, "secret")
	readText(t, c) // presence
	if err := c.WriteMessage(wsock.OpText, []byte(`{"role":"phone"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(wsock.OpText, []byte(`{"type":"hello","session_id":"s-1"}`)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readText(t, c)), &got); err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "ack" {
		t.Fatalf("answer to hello = %v, want the hub's ack", got)
	}
}

func TestDirectSocketServesRPC(t *testing.T) {
	var seen atomicString
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.set(r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	srv, _ := directServer(t, "secret", api)
	c := dialDirect(t, srv, "secret")
	readText(t, c) // presence

	frame := map[string]any{"ch": "rpc", "rid": "r-1", "m": "GET", "p": "/v1/models?provider=x"}
	data, _ := json.Marshal(frame)
	if err := c.WriteMessage(wsock.OpText, data); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(readText(t, c)), &got); err != nil {
		t.Fatal(err)
	}
	if got["ch"] != "rpc" || got["rid"] != "r-1" {
		t.Fatalf("reply = %v", got)
	}
	if got["s"] != float64(200) {
		t.Fatalf("status = %v, want 200", got["s"])
	}
	body, _ := base64.StdEncoding.DecodeString(got["b"].(string))
	if string(body) != `{"data":[]}` {
		t.Fatalf("body = %q", body)
	}
	if want := "GET /v1/models?provider=x Bearer secret"; seen.get() != want {
		t.Fatalf("api saw %q, want %q", seen.get(), want)
	}
}

// A request that arrives in several frames only reaches the mux on the last
// one, with every chunk joined: half a JSON body must never be dispatched.
func TestDirectSocketJoinsRPCChunks(t *testing.T) {
	var seen atomicString
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		seen.set(string(buf[:n]))
		_, _ = w.Write([]byte(`{}`))
	})
	srv, _ := directServer(t, "secret", api)
	c := dialDirect(t, srv, "secret")
	readText(t, c) // presence

	first, _ := json.Marshal(map[string]any{
		"ch": "rpc", "rid": "r-2", "more": true,
		"b": base64.StdEncoding.EncodeToString([]byte(`{"a":`)),
	})
	last, _ := json.Marshal(map[string]any{
		"ch": "rpc", "rid": "r-2", "m": "POST", "p": "/v1/echo",
		"b": base64.StdEncoding.EncodeToString([]byte(`1}`)),
	})
	if err := c.WriteMessage(wsock.OpText, first); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(wsock.OpText, last); err != nil {
		t.Fatal(err)
	}
	readText(t, c) // the rpc reply
	if want := `{"a":1}`; seen.get() != want {
		t.Fatalf("api saw %q, want %q", seen.get(), want)
	}
}

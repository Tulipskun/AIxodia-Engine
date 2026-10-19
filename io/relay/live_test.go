package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aixodia-engine/io/mobile"
	"aixodia-engine/io/wsock"
)

// TestLiveRelayRoundTrip drives the deployed Worker and Durable Object with the
// same client code the engine runs. It is opt-in: without RELAY_LIVE_URL the
// relay suite stays offline.
//
//	RELAY_LIVE_URL=wss://aixodia-relay.aixodia.workers.dev RELAY_LIVE_TOKEN=... \
//	  go test ./io/relay -run TestLive -v
func TestLiveRelayRoundTrip(t *testing.T) {
	base := strings.TrimRight(os.Getenv("RELAY_LIVE_URL"), "/")
	if base == "" {
		t.Skip("RELAY_LIVE_URL not set")
	}
	token := os.Getenv("RELAY_LIVE_TOKEN")
	if token == "" {
		token = "test-token"
	}
	room := "live-" + time.Now().Format("150405") + "-" + strings.ToLower(base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano))))
	roomURL := base + "/room/" + room

	api := http.NewServeMux()
	api.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pong":true}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	engineUp := make(chan struct{}, 1)
	engine := make(chan error, 1)
	go func() {
		engine <- Run(ctx, Config{
			URL: roomURL, Token: token, Started: 1000, Version: "live-test",
			Hub: mobile.NewHub(),
			Chat: func(context.Context, string, string, []mobile.Attachment, func(string), func(mobile.ToolEvent)) (mobile.ChatResult, error) {
				return mobile.ChatResult{}, nil
			},
			API: api,
			OnConnect: func() {
				select {
				case engineUp <- struct{}{}:
				default:
				}
			},
		})
	}()
	select {
	case <-engineUp:
	case err := <-engine:
		t.Fatalf("engine never connected: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("engine never connected: timed out")
	}

	phone, err := wsock.Dial(ctx, roomURL, authHeader(token))
	if err != nil {
		t.Fatalf("phone dial: %v", err)
	}
	defer phone.Close()
	if err := phone.WriteMessage(wsock.OpText, []byte(`{"role":"phone"}`)); err != nil {
		t.Fatalf("phone hello: %v", err)
	}

	waitFrame(t, phone, 30*time.Second, func(m map[string]any) bool {
		return m["ctl"] == "presence" && m["engine"] == true
	})

	// RPC through the real Worker, answered by the fake engine.
	if err := phone.WriteMessage(wsock.OpText, []byte(`{"ch":"rpc","rid":"r1","m":"GET","p":"/ping"}`)); err != nil {
		t.Fatalf("rpc send: %v", err)
	}
	reply := waitFrame(t, phone, 30*time.Second, func(m map[string]any) bool {
		return m["ch"] == "rpc" && m["rid"] == "r1"
	})
	body, _ := base64.StdEncoding.DecodeString(reply["b"].(string))
	if string(body) != `{"pong":true}` {
		t.Fatalf("rpc body = %q", body)
	}

	// Chat frames arrive at the engine with the channel key intact.
	if err := phone.WriteMessage(wsock.OpText, []byte(`{"ch":"chat","type":"hello","session_id":"s1"}`)); err != nil {
		t.Fatalf("chat send: %v", err)
	}
	ack := waitFrame(t, phone, 30*time.Second, func(m map[string]any) bool { return m["kind"] == "ack" })
	if ack["ch"] != "chat" || ack["session_id"] != "s1" {
		t.Fatalf("ack = %+v", ack)
	}

	// A newer engine takes the room and the old one is told to exit.
	second := make(chan error, 1)
	go func() {
		second <- Run(ctx, Config{
			URL: roomURL, Token: token, Started: 2000, Version: "live-test-2",
			Hub: mobile.NewHub(),
			API: api,
		})
	}()
	select {
	case err := <-engine:
		if err != ErrSuperseded {
			t.Fatalf("first engine = %v, want ErrSuperseded", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("first engine was not superseded")
	}
	select {
	case err := <-second:
		t.Fatalf("second engine exited: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
}

func authHeader(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

func waitFrame(t *testing.T, c *wsock.Client, timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(deadline)
		op, payload, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if op != wsock.OpText {
			continue
		}
		var m map[string]any
		if json.Unmarshal(payload, &m) != nil {
			continue
		}
		if pred(m) {
			return m
		}
	}
	t.Fatal("frame never arrived")
	return nil
}

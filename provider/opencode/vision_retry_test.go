package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aixodia-engine/provider"
)

// fakeUpstream answers the way a text-only model does: normally without a
// picture, and with an upstream error when one is attached. This is the shape
// ling-3.1-flash-free showed on the live endpoint — it does not politely refuse,
// it falls over.
func fakeUpstream(t *testing.T, imageStatus int, imageBody string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls++
		hasImage := strings.Contains(string(raw), "image_url") || strings.Contains(string(raw), "input_image")
		if hasImage {
			w.WriteHeader(imageStatus)
			_, _ = w.Write([]byte(imageBody))
			return
		}
		writeSSE(w, r, "ตอบจากข้อความล้วน")
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// writeSSE answers the way the streaming endpoint does. The adapter's first
// attempt is always a stream, so a plain JSON body would be read as an empty
// successful answer and the retry would look like it produced nothing.
func writeSSE(w http.ResponseWriter, r *http.Request, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	var body string
	if strings.HasSuffix(r.URL.Path, "/responses") {
		// The Responses API answers in events of its own shape. Serving the
		// chat shape here would be read as an empty answer and would make the
		// fallback look broken.
		delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": content})
		done, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{"id": "resp-1", "model": "m",
				"output": []map[string]any{{"type": "message",
					"content": []map[string]any{{"type": "output_text", "text": content}}}}},
		})
		body = "data: " + string(delta) + "\n\ndata: " + string(done) + "\n\n"
	} else {
		payload, _ := json.Marshal(map[string]any{
			"id": "x", "model": "m",
			"choices": []map[string]any{{"delta": map[string]any{"content": content}}},
		})
		body = "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
	}
	_, _ = w.Write([]byte(body))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func imageRequest() *provider.Request {
	return &provider.Request{
		Model: "text-only-model",
		Messages: []provider.Message{
			{Role: "user", Content: "รูปนี้ใช่คนเดียวกันมั้ย?", Images: []provider.Image{
				{MediaType: "image/jpeg", Data: []byte{1, 2, 3, 4}},
			}},
		},
	}
}

func TestStreamDropsPicturesWhenTheModelFallsOver(t *testing.T) {
	// 502 with a body that names nothing useful, which is exactly what the live
	// endpoint returned and what the old word-matching rule could not read.
	server, calls := fakeUpstream(t, http.StatusBadGateway, "error code: 502")
	prov := provider.Provider{Name: "fake", Endpoint: server.URL, Keys: []string{"k"}}

	resp, err := New().Complete(context.Background(), imageRequest(), prov, "k")
	if err != nil {
		t.Fatalf("the retry should have answered: %v", err)
	}
	if resp.Content != "ตอบจากข้อความล้วน" {
		t.Errorf("content = %q, want the text-only answer", resp.Content)
	}
	if *calls < 2 {
		t.Errorf("the upstream was called %d times; the picture request should have been followed by one without", *calls)
	}
}

func TestStreamDoesNotRetryWhenTheKeyIsTheProblem(t *testing.T) {
	server, calls := fakeUpstream(t, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	prov := provider.Provider{Name: "fake", Endpoint: server.URL, Keys: []string{"k"}}

	if _, err := New().Complete(context.Background(), imageRequest(), prov, "k"); err == nil {
		t.Fatal("a bad key must fail")
	}
	// Retrying would spend another round trip on the same failure.
	if *calls > 1 {
		t.Errorf("a bad key should not be retried, upstream was called %d times", *calls)
	}
}

func TestStreamKeepsPicturesWhenTheModelTakesThem(t *testing.T) {
	var sawImage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "image_url") || strings.Contains(string(raw), "input_image") {
			sawImage = true
		}
		writeSSE(w, r, "เห็นภาพแล้ว")
	}))
	defer server.Close()
	prov := provider.Provider{Name: "fake", Endpoint: server.URL, Keys: []string{"k"}}

	resp, err := New().Complete(context.Background(), imageRequest(), prov, "k")
	if err != nil {
		t.Fatalf("a model that takes pictures should answer: %v", err)
	}
	if !sawImage {
		t.Error("the picture should have been sent on the first attempt")
	}
	if resp.Content != "เห็นภาพแล้ว" {
		t.Errorf("content = %q", resp.Content)
	}
}

// The retry has to be told why, or the answer reads as though the picture was
// seen — the failure this whole path exists to avoid.
func TestStreamTellsTheMessagePicturesWereDropped(t *testing.T) {
	var sawNote bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if strings.Contains(body, "could not see") {
			sawNote = true
		}
		if strings.Contains(body, "image_url") || strings.Contains(body, "input_image") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("error code: 502"))
			return
		}
		writeSSE(w, r, "ผมมองไม่เห็นรูปครับ")
	}))
	defer server.Close()
	prov := provider.Provider{Name: "fake", Endpoint: server.URL, Keys: []string{"k"}}

	if _, err := New().Complete(context.Background(), imageRequest(), prov, "k"); err != nil {
		t.Fatalf("the retry should have answered: %v", err)
	}
	if !sawNote {
		t.Error("the retried request must say the pictures were dropped")
	}
}

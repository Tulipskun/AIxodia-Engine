package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-engine/provider"
)

func TestUserFieldForwarded(t *testing.T) {
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	a := &Adapter{}
	_, err := a.Stream(context.Background(), &provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		User:     "opencode-user",
	}, provider.Provider{Name: "p", Endpoint: server.URL, Keys: []string{"k"}}, "k", func(string) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var parsed struct {
		User string `json:"user"`
	}
	_ = json.Unmarshal(seen, &parsed)
	if parsed.User != "opencode-user" {
		t.Errorf("user = %q, want opencode-user (body: %s)", parsed.User, seen)
	}
}

func TestUserOmittedWhenEmpty(t *testing.T) {
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	a := &Adapter{}
	_, err := a.Stream(context.Background(), &provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, provider.Provider{Name: "p", Endpoint: server.URL, Keys: []string{"k"}}, "k", func(string) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var parsed struct {
		User *string `json:"user"`
	}
	_ = json.Unmarshal(seen, &parsed)
	if parsed.User != nil {
		t.Errorf("empty user should be omitted, got %q (body: %s)", *parsed.User, seen)
	}
}

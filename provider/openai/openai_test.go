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

func TestBuildRequestStreamOptionsAndToolChoice(t *testing.T) {
	withTools := buildRequest(&provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "a", Description: "b"}},
	}, true)
	if withTools.StreamOptions != nil {
		t.Errorf("streaming request must omit stream_options, got %+v", withTools.StreamOptions)
	}
	if withTools.ToolChoice != "auto" {
		t.Errorf("request with tools should set tool_choice=auto, got %v", withTools.ToolChoice)
	}

	plain := buildRequest(&provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, false)
	if plain.StreamOptions != nil {
		t.Errorf("non-streaming request must omit stream_options, got %+v", plain.StreamOptions)
	}
	if plain.ToolChoice != nil {
		t.Errorf("tools-less request must omit tool_choice, got %v", plain.ToolChoice)
	}
	raw, _ := json.Marshal(plain)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"stream_options", "tool_choice", "tools", "user"} {
		if _, ok := m[k]; ok {
			t.Errorf("plain request should omit %q, body: %s", k, raw)
		}
	}
}

func TestZZResponsesToolHistoryMapping(t *testing.T) {
	req := &provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: "user", Content: "run echo hi"},
			{Role: "assistant", Content: "on it", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"echo hi"}`}}},
			{Role: "tool", ToolCallID: "call_1", Content: "hi\n"},
		},
		MaxTokens: 100,
		Tools:     []provider.ToolDef{{Name: "bash"}},
	}
	body := buildResponsesRequest(req, true)
	items, ok := body.Input.([]any)
	if !ok || len(items) != 4 {
		t.Fatalf("input items = %#v", body.Input)
	}
	fc, _ := items[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "bash" {
		t.Errorf("function_call item = %v", fc)
	}
	fco, _ := items[3].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" {
		t.Errorf("function_call_output item = %v", fco)
	}
}

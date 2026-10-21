package anthropic

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

func fakeServer(t *testing.T, handler http.HandlerFunc) provider.Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return provider.Provider{Name: "claude", Endpoint: server.URL, Keys: []string{"k"}, Adapter: "anthropic"}
}

func TestBuildRequestGroupsToolResults(t *testing.T) {
	req := &provider.Request{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "time and weather?"},
			{Role: "assistant", Content: "checking", ToolCalls: []provider.ToolCall{
				{ID: "a", Name: "current_time", Arguments: "{}"},
				{ID: "b", Name: "weather", Arguments: `{"city":"Bangkok"}`},
			}},
			{Role: "tool", ToolCallID: "a", Content: "10:00"},
			{Role: "tool", ToolCallID: "b", Content: "hot"},
		},
	}
	out := buildRequest(req, false)

	if out.System != "be brief" {
		t.Errorf("system = %q", out.System)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %d, want user, assistant, user(tool_results)", len(out.Messages))
	}
	last := out.Messages[2]
	if last.Role != "user" || len(last.Content) != 2 || last.Content[0].Type != "tool_result" || last.Content[1].ToolUseID != "b" {
		t.Errorf("tool results not grouped: %+v", last)
	}
	assistant := out.Messages[1]
	if assistant.Content[1].Type != "tool_use" || string(assistant.Content[2].Input) != `{"city":"Bangkok"}` {
		t.Errorf("tool_use blocks wrong: %+v", assistant.Content)
	}
}

func TestCompleteParsesToolUse(t *testing.T) {
	prov := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, `{"error":{"message":"bad headers"}}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","stop_reason":"tool_use",
			"content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"t1","name":"current_time","input":{}}],
			"usage":{"input_tokens":5,"output_tokens":7}}`)
	})
	resp, err := New().Complete(context.Background(), &provider.Request{Model: "claude-x"}, prov, "k")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "let me check" || resp.FinishReason != "tool_calls" {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "current_time" || resp.ToolCalls[0].Arguments != "{}" {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// Anthropic reports the prompt in three parts. input_tokens is only the
// uncached part, so a turn that was almost entirely cache-read used to look
// like a tiny fresh prompt and a cache share of zero.
func TestCompleteKeepsCacheReadAndWriteBesideThePrompt(t *testing.T) {
	prov := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"msg_c","model":"claude-x","stop_reason":"end_turn",
			"content":[{"type":"text","text":"hi"}],
			"usage":{"input_tokens":20,"output_tokens":5,
			         "cache_read_input_tokens":900,"cache_creation_input_tokens":80}}`)
	})
	resp, err := New().Complete(context.Background(), &provider.Request{Model: "claude-x"}, prov, "k")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 20 || resp.Usage.CacheReadTokens != 900 ||
		resp.Usage.CacheWriteTokens != 80 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.Usage.InputIncludesCache {
		t.Error("Anthropic's input_tokens does not contain the cache; the flag must stay false")
	}
	if resp.Usage.TotalTokens != 1005 {
		t.Errorf("total = %d, want the whole prompt plus output (1005)", resp.Usage.TotalTokens)
	}
}

func TestStreamEmitsTextAndAssemblesToolInput(t *testing.T) {
	prov := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_2","model":"claude-x","usage":{"input_tokens":3}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t9","name":"current_time"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zone\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"UTC\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		}
		for _, e := range events {
			_, _ = io.WriteString(w, "event: x\ndata: "+e+"\n\n")
		}
	})
	var streamed strings.Builder
	resp, err := New().Stream(context.Background(), &provider.Request{Model: "claude-x", Stream: true}, prov, "k",
		func(text string) error { streamed.WriteString(text); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello" || resp.Content != "Hello" {
		t.Errorf("streamed = %q, content = %q", streamed.String(), resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"zone":"UTC"}` {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage.CompletionTokens != 4 || resp.FinishReason != "tool_calls" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestErrorStatusBecomesAPIError(t *testing.T) {
	prov := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "rate limited"}})
	})
	_, err := New().Complete(context.Background(), &provider.Request{Model: "x"}, prov, "k")
	api, ok := err.(*provider.APIError)
	if !ok || api.StatusCode != 429 || api.Message != "rate limited" {
		t.Fatalf("err = %#v", err)
	}
	if !provider.Retryable(err) {
		t.Error("429 should be retryable")
	}
}

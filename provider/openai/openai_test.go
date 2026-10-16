package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-engine/provider"
)

// A reasoning model writes its thinking before the answer. The engine used to
// drop it, so the assistant turn it replayed on the next request had no
// reasoning_content and a provider that validates one refuses the whole
// conversation: 400 Missing required parameter: 'messages[1].reasoning_content'.
func TestReasoningContentTravelsWithTheMessage(t *testing.T) {
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"คิดก่อน\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ตอบแล้ว\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	a := &Adapter{}
	out, err := a.Stream(context.Background(), &provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "ตอบแล้ว", ReasoningContent: "คิดก่อน"},
			{Role: "user", Content: "ต่อ"},
		},
	}, provider.Provider{Name: "p", Endpoint: server.URL, Keys: []string{"k"}}, "k", func(string) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if out.ReasoningContent != "คิดก่อน" {
		t.Errorf("response reasoning = %q, want the reasoning the provider sent", out.ReasoningContent)
	}
	if !strings.Contains(string(seen), `"reasoning_content":"คิดก่อน"`) {
		t.Errorf("replayed assistant turn carries no reasoning: %s", seen)
	}
	if strings.Count(string(seen), "reasoning_content") != 1 {
		t.Errorf("only the assistant turn should carry it: %s", seen)
	}
}

// The gateway that produced the error only checks that the key is there. One
// retry with it present and empty is enough to hold the conversation.
func TestMissingReasoningKeyIsRepairedByOneRetry(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if !strings.Contains(string(body), "reasoning_content") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Missing required parameter: 'messages[1].reasoning_content'"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ตอบแล้ว\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	a := &Adapter{}
	out, err := a.Stream(context.Background(), &provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "คำตอบก่อนหน้า"},
			{Role: "user", Content: "ต่อ"},
		},
	}, provider.Provider{Name: "p", Endpoint: server.URL, Keys: []string{"k"}}, "k", func(string) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if out.Content != "ตอบแล้ว" {
		t.Errorf("content = %q, want the answer from the retry", out.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want the original plus one repair", len(bodies))
	}
	if !strings.Contains(bodies[1], `"reasoning_content":""`) {
		t.Errorf("the repair must send the key on the assistant turn: %s", bodies[1])
	}
}

// Any other 400 is the caller's problem and is reported unchanged, without a
// second request.
func TestOtherBadRequestIsNotRetried(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model does not exist"}}`)
	}))
	t.Cleanup(server.Close)

	a := &Adapter{}
	_, err := a.Complete(context.Background(), &provider.Request{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, provider.Provider{Name: "p", Endpoint: server.URL, Keys: []string{"k"}}, "k")
	apiErr, ok := err.(*provider.APIError)
	if !ok {
		t.Fatalf("error = %v, want an APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(apiErr.Message, "model does not exist") {
		t.Errorf("error = %+v, want the upstream refusal unchanged", apiErr)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want the failure reported without a retry", requests)
	}
}

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

// The Responses API reports what the cache served and wrote in the input
// details, and what the model thought in the output details. A parser that
// reads only the top-level counts shows a cache that never works.
func TestResponsesUsageCarriesCacheAndThinking(t *testing.T) {
	raw := `{"id":"resp-1","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],` +
		`"usage":{"input_tokens":100,"output_tokens":5,"total_tokens":105,` +
		`"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":10},` +
		`"output_tokens_details":{"reasoning_tokens":2}}}`
	var parsed responsesResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, _, usage := parseResponsesOutput(parsed)
	if usage.CacheReadTokens != 80 {
		t.Errorf("cache read = %d, want 80", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 10 {
		t.Errorf("cache write = %d, want 10", usage.CacheWriteTokens)
	}
	if usage.ReasoningTokens != 2 {
		t.Errorf("reasoning = %d, want 2", usage.ReasoningTokens)
	}
	if !usage.InputIncludesCache {
		t.Error("input_tokens already contains the cache part; the flag must say so")
	}
}

// The prompt cache key names the conversation, so every request in one chat
// carries the same key. A key that changes per request scatters one prefix
// across buckets and guarantees a cache miss.
func TestResponsesCacheKeyNamesTheSession(t *testing.T) {
	first := buildResponsesRequest(&provider.Request{Model: "m", SessionID: "s1"}, true)
	same := buildResponsesRequest(&provider.Request{Model: "m", SessionID: "s1"}, true)
	if first.PromptCacheKey == "" {
		t.Fatal("a request with a session must carry a prompt cache key")
	}
	if first.PromptCacheKey != same.PromptCacheKey {
		t.Errorf("cache key changed between requests: %q then %q", first.PromptCacheKey, same.PromptCacheKey)
	}

	other := buildResponsesRequest(&provider.Request{Model: "m", SessionID: "s2"}, true)
	if other.PromptCacheKey == first.PromptCacheKey {
		t.Errorf("a different session must key a different prefix, both = %q", other.PromptCacheKey)
	}

	none := buildResponsesRequest(&provider.Request{Model: "m"}, true)
	if none.PromptCacheKey != "" {
		t.Errorf("a request without a session must send no key, got %q", none.PromptCacheKey)
	}
}

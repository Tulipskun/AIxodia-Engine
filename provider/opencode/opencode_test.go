package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"ai-engine/provider"
)

type capturedRequest struct {
	Header http.Header
	Body   []byte
}

func fakeZen(t *testing.T) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var seen []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, capturedRequest{Header: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"gen-1","model":"zen-model","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"Hello "}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"gen-1","model":"zen-model","choices":[{"index":0,"finish_reason":"stop","delta":{"role":"assistant","content":"there"}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func providerFor(server *httptest.Server) provider.Provider {
	return provider.Provider{Name: "opencode", Endpoint: server.URL, Keys: []string{"k"}}
}

func toolNames(body []byte) []string {
	var parsed struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &parsed)
	names := make([]string, 0, len(parsed.Tools))
	for _, entry := range parsed.Tools {
		names = append(names, entry.Function.Name)
	}
	return names
}

func hasStream(body []byte) bool {
	var parsed struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Stream
}

func TestFreeTierContractOnComplete(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()

	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]

	if ua := got.Header.Get("User-Agent"); ua != "opencode/1.18.31" {
		t.Errorf("User-Agent = %q", ua)
	}
	session := got.Header.Get("X-Opencode-Session")
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(session) {
		t.Errorf("session = %q", session)
	}
	if request := got.Header.Get("X-Opencode-Request"); !strings.HasPrefix(request, "msg_") {
		t.Errorf("request = %q", request)
	}
	if client := got.Header.Get("X-Opencode-Client"); client != "cli" {
		t.Errorf("client = %q", client)
	}
	if project := got.Header.Get("X-Opencode-Project"); project != "global" {
		t.Errorf("project = %q", project)
	}
	if !hasStream(got.Body) {
		t.Errorf("stream = false in body: %s", got.Body)
	}

	names := toolNames(got.Body)
	for _, want := range []string{"current_time", "shell", "read"} {
		if !contains(names, want) {
			t.Errorf("tools = %v, missing %q", names, want)
		}
	}

	if resp.Content != "Hello there" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 33 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestStreamKeepsGivenContractTools(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()

	var streamed strings.Builder
	_, err := adapter.Stream(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "bash"}, {Name: "read"}},
	}, providerFor(server), "key", func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if streamed.String() != "Hello there" {
		t.Errorf("emitted = %q", streamed.String())
	}
	names := toolNames((*seen)[0].Body)
	if strings.Join(names, ",") != "bash,read" {
		t.Errorf("tools = %v, want exactly bash,read", names)
	}
}

func TestSessionStableAndRequestRotates(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()
	prov := providerFor(server)

	for i := 0; i < 2; i++ {
		if _, err := adapter.Complete(context.Background(), &provider.Request{
			Model:    "zen-model",
			Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, prov, "key"); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
	first, second := (*seen)[0].Header, (*seen)[1].Header
	if first.Get("X-Opencode-Session") != second.Get("X-Opencode-Session") {
		t.Errorf("session rotated: %q vs %q", first.Get("X-Opencode-Session"), second.Get("X-Opencode-Session"))
	}
	if first.Get("X-Opencode-Request") == second.Get("X-Opencode-Request") {
		t.Errorf("request id reused: %q", first.Get("X-Opencode-Request"))
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestContractToolsAreTruthful(t *testing.T) {
	got := contractTools(nil)
	names := map[string]provider.ToolDef{}
	for _, d := range got {
		names[d.Name] = d
	}
	for _, name := range []string{"shell", "read"} {
		d, ok := names[name]
		if !ok {
			t.Fatalf("contractTools(nil) missing %q", name)
		}
		if strings.Contains(d.Description, "Not available") {
			t.Errorf("placeholder %q still discourages use: %q", name, d.Description)
		}
		if len(d.Parameters) == 0 {
			t.Errorf("placeholder %q has no parameters", name)
		}
	}
}

func TestRetryWithoutToolsOnProtocolRejection(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if calls == 1 {
			if !strings.Contains(string(body), `"shell"`) {
				t.Errorf("first request should carry injected tools")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
			return
		}
		var parsed struct {
			Tools []any `json:"tools"`
		}
		_ = json.Unmarshal(body, &parsed)
		if parsed.Tools != nil {
			t.Errorf("retry should carry no tools, got %v", parsed.Tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	adapter := New()
	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("retry without tools: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q, want recovered", resp.Content)
	}
	if calls != 2 {
		t.Errorf("requests = %d, want 2 (initial + tools-less retry)", calls)
	}
}

func TestNoRetryOnUnrelatedErrors(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"message":"Insufficient account funds"}`)
	}))
	t.Cleanup(server.Close)

	adapter := New()
	_, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, providerFor(server), "key")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1 (no retry on funds errors)", calls)
	}
}

func TestRetryNonStreamingWhenBareStreamAlsoRejected(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Stream bool  `json:"stream"`
			Tools  []any `json:"tools"`
		}
		_ = json.Unmarshal(body, &parsed)
		if parsed.Stream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
			return
		}
		if parsed.Tools != nil {
			t.Errorf("non-stream retry should carry no tools, got %v", parsed.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gen-9","model":"zen-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"plain ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	t.Cleanup(server.Close)

	adapter := New()
	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("non-stream fallback: %v", err)
	}
	if resp.Content != "plain ok" {
		t.Errorf("content = %q, want plain ok", resp.Content)
	}
	if calls != 3 {
		t.Errorf("requests = %d, want 3 (tools+stream, bare stream, bare plain)", calls)
	}
}

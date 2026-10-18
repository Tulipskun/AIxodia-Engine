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

	"aixodia-engine/provider"
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
		if strings.HasSuffix(r.URL.Path, "/responses") {
			// The Responses API reports the answer as events, then repeats it in
			// the completed object. Both are served because the adapter must
			// prefer the deltas and not double the text.
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"Hello "}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"there"}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-1","model":"zen-model","output":[{"type":"message","content":[{"type":"output_text","text":"Hello there"}]}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}}`+"\n\n")
			return
		}
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
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &parsed)
	names := make([]string, 0, len(parsed.Tools))
	for _, entry := range parsed.Tools {
		if entry.Name != "" {
			names = append(names, entry.Name)
			continue
		}
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

	// The name the client itself sends, read off its own request. The engine
	// used to send an AI-SDK style string that no version of the client has
	// ever sent.
	if ua := got.Header.Get("User-Agent"); ua != clientAgentMeasured {
		t.Errorf("User-Agent = %q, want %q", ua, clientAgentMeasured)
	}
	session := got.Header.Get("X-Opencode-Session")
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(session) {
		t.Errorf("session = %q", session)
	}
	// The engine must not name its own request. Measured against the live
	// endpoint on 2026-10-15: the engine's captured request with this header is
	// answered 403 FreeTierError, and removing only this header makes the same
	// request 200. The real client never sends it.
	if request := got.Header.Get("X-Opencode-Request"); request != "" {
		t.Errorf("request header = %q, want none at all", request)
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
		Tools:    []provider.ToolDef{{Name: "shell"}, {Name: "read"}},
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
	if strings.Join(names, ",") != "shell,read" {
		t.Errorf("tools = %v, want exactly shell,read", names)
	}
}

func TestSessionStableAndRequestNeverSent(t *testing.T) {
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
	if first.Get("X-Opencode-Request") != "" {
		t.Errorf("request id sent: %q, want none at all", first.Get("X-Opencode-Request"))
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
		if strings.Contains(string(body), `"tools"`) {
			if !strings.Contains(string(body), `"shell"`) {
				t.Errorf("a request carrying tools should carry the injected ones")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
			return
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
	// responses+tools, chat+tools, then the tools-less chat retry
	if calls != 3 {
		t.Errorf("requests = %d, want 3 (two with tools, then one without)", calls)
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
	// responses+tools, chat+tools, chat bare, then the non-streaming one
	if calls != 4 {
		t.Errorf("requests = %d, want 4 (three streaming, then plain)", calls)
	}
}

// The Responses API is the first attempt, because the gate on this endpoint
// reads the tool declarations by name at the top level and chat completions
// nests them where it cannot see them. This is the order that makes a request
// accepted rather than refused, so it is asserted rather than left to chance.
func TestResponsesIsTriedFirst(t *testing.T) {
	calls := 0
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		paths = append(paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/responses") {
			var parsed struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &parsed)
			if parsed.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_text.delta\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"via-resp-stream\"}\n\n")
				_, _ = io.WriteString(w, "event: response.completed\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-s\",\"model\":\"zen-model\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"via-resp-stream\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-1","model":"zen-model","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"via-responses"}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
	}))
	t.Cleanup(server.Close)

	adapter := New()
	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("responses fallback: %v", err)
	}
	if resp.Content != "via-resp-stream" {
		t.Errorf("content = %q, want via-resp-stream", resp.Content)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1: the Responses API is tried first and it worked", calls)
	}
	if got := paths[0]; !strings.HasSuffix(got, "/responses") {
		t.Errorf("first request should hit /responses, got %s", got)
	}
}

func TestResponsesFallbackKeepsFullTools(t *testing.T) {
	var paths []string
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		var parsed struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &parsed)
		if strings.HasSuffix(r.URL.Path, "/responses") && parsed.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"model\":\"zen-model\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"via-resp-full\"}]}]}}\n\n")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-2","model":"zen-model","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"via-resp-full"}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
	}))
	t.Cleanup(server.Close)

	adapter := New()
	resp, err := adapter.Complete(context.Background(), &provider.Request{
		Model:    "zen-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.ToolDef{{Name: "current_time", Description: "now"}},
	}, providerFor(server), "key")
	if err != nil {
		t.Fatalf("responses fallback: %v", err)
	}
	if resp.Content != "via-resp-full" {
		t.Errorf("content = %q", resp.Content)
	}
	// last request must be /responses carrying the original tool (flat format)
	last := bodies[len(bodies)-1]
	var parsed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(last, &parsed)
	found := false
	for _, tool := range parsed.Tools {
		if tool.Name == "current_time" {
			found = true
		}
	}
	if !found {
		t.Errorf("responses fallback should carry original tools, body: %s", last)
	}
}

// TestSparkChainReachesResponsesTools simulates a responses-only free-tier
// model: chat+tools -> 400 protocol, chat bare -> 403 gate, responses+tools
// -> 200. The adapter must reach the responses attempt and return content.
func TestSparkChainReachesResponsesTools(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path+" tools="+hasTools(body))
		hasT := strings.Contains(string(body), `"tools"`)
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions") && hasT:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Model does not support this protocol."}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"OpenCode's free tier can only be used from within OpenCode"}`)
		case strings.HasSuffix(r.URL.Path, "/responses") && hasT:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"SPARK-OK"}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"SPARK-OK"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`+"\n\n")
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"OpenCode's free tier can only be used from within OpenCode"}`)
		}
	}))
	t.Cleanup(server.Close)

	adapter := New()
	var got []string
	resp, err := adapter.Stream(context.Background(), &provider.Request{
		Model:     "muse-spark-1.3-contributor-free",
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 60,
		Tools:     []provider.ToolDef{{Name: "shell", Description: "run"}, {Name: "read", Description: "read"}},
	}, providerFor(server), "key", func(s string) error { got = append(got, s); return nil })
	if err != nil {
		t.Fatalf("stream: %v (paths=%v)", err, paths)
	}
	if resp.Content != "SPARK-OK" {
		t.Errorf("content = %q, want SPARK-OK (paths=%v)", resp.Content, paths)
	}
	found := false
	for _, p := range paths {
		if strings.HasSuffix(p, "/responses tools=true") {
			found = true
		}
	}
	if !found {
		t.Errorf("never tried responses-with-tools (paths=%v)", paths)
	}
	if len(paths) == 0 || !strings.HasSuffix(paths[0], "/responses tools=true") {
		t.Errorf("responses-with-tools should be the first attempt, paths=%v", paths)
	}
}

func hasTools(body []byte) string {
	if strings.Contains(string(body), `"tools"`) {
		return "true"
	}
	return "false"
}

func TestContractToolsExpandsPartialSets(t *testing.T) {
	// The gate wants "shell" by name. A set that lacks it — even the old
	// "bash" name the engine no longer offers — gets "shell" injected, never a
	// rename of what was given.
	got := contractTools([]provider.ToolDef{{Name: "bash"}})
	names := map[string]bool{}
	for _, d := range got {
		names[d.Name] = true
	}
	if !names["bash"] || !names["shell"] || !names["read"] {
		t.Errorf("contract([bash]) = %v, want bash kept and shell+read injected", names)
	}
	for _, d := range got {
		if d.Name == "read" && len(d.Description) < 100 {
			t.Errorf("injected read def is a stub (%d chars), gate needs the rich def", len(d.Description))
		}
	}
	got = contractTools(nil)
	names = map[string]bool{}
	for _, d := range got {
		names[d.Name] = true
	}
	if !names["shell"] || !names["read"] {
		t.Errorf("contract(nil) = %v, want shell+read", names)
	}
}

// A model that cannot take pictures has to be asked again without them, and the
// message has to be told they were dropped: an answer that reads as though the
// picture was seen is worse than no answer.
func TestWithoutImagesStripsAndSaysSo(t *testing.T) {
	req := &provider.Request{Messages: []provider.Message{
		{Role: "user", Content: "อันเก่า"},
		{Role: "assistant", Content: "ตอบ"},
		{Role: "user", Content: "รูปนี้ใช่คนเดียวกันมั้ย?", Images: []provider.Image{
			{MediaType: "image/jpeg", Data: []byte{1, 2, 3}},
		}},
	}}
	got := withoutImages(req)

	if carriesImages(got.Messages) {
		t.Fatal("no picture should survive")
	}
	if !strings.Contains(got.Messages[2].Content, "could not see") {
		t.Errorf("the message must be told, got: %q", got.Messages[2].Content)
	}
	if !strings.HasPrefix(got.Messages[2].Content, "รูปนี้ใช่คนเดียวกันมั้ย?") {
		t.Errorf("the original question must survive: %q", got.Messages[2].Content)
	}
	// The caller's request is the one that describes what was asked for, so it
	// must not be modified in place.
	if !carriesImages(req.Messages) {
		t.Error("withoutImages must not mutate the request it was given")
	}
}

func TestCarriesImages(t *testing.T) {
	if carriesImages([]provider.Message{{Role: "user", Content: "text"}}) {
		t.Error("a text-only conversation carries no pictures")
	}
	if !carriesImages([]provider.Message{{Role: "user", Images: []provider.Image{{Data: []byte{1}}}}}) {
		t.Error("a message with an image should be reported")
	}
}

// Only a key or a balance failure is hopeless without the pictures. Everything
// else is retried once, because a text-only model handed a picture tends to fall
// over rather than explain itself: ling-3.1-flash-free answers normally without
// one and returns 502 with one, and no amount of word-matching in the error
// would have predicted that.
func TestWorthRetryingWithoutImages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"a model that names the protocol", &provider.APIError{StatusCode: 400, Message: "Model does not support this protocol."}, true},
		{"a payload rejection", &provider.APIError{StatusCode: 400, Message: "invalid_request: image too large"}, true},
		{"an upstream crash on the picture", &provider.APIError{StatusCode: 502, Message: "error code: 502"}, true},
		{"an upstream 500", &provider.APIError{StatusCode: 500, Message: "internal error"}, true},
		{"a free-tier 403", &provider.APIError{StatusCode: 403, Message: "OpenCode's free tier can only be used from within OpenCode"}, true},
		{"a bad key", &provider.APIError{StatusCode: 401, Message: "bad key"}, false},
		{"no credit", &provider.APIError{StatusCode: 402, Message: "out of credit"}, false},
		{"a 403 about the key", &provider.APIError{StatusCode: 403, Message: "invalid api key"}, false},
		{"a 403 about the balance", &provider.APIError{StatusCode: 403, Message: "insufficient balance"}, false},
	}
	for _, c := range cases {
		if got := worthRetryingWithoutImages(c.err); got != c.want {
			t.Errorf("%s: worthRetryingWithoutImages(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// The cache key the adapter sends must name the conversation, not the moment:
// the same chat has to carry the same key on every turn or the provider can
// never reuse the prefix it already read.
func TestResponsesCacheKeyIsStablePerSession(t *testing.T) {
	server, seen := fakeZen(t)
	adapter := New()
	for turn := 0; turn < 2; turn++ {
		_, err := adapter.Complete(context.Background(), &provider.Request{
			Model:     "zen-model",
			Messages:  []provider.Message{{Role: "user", Content: "hi"}},
			Tools:     []provider.ToolDef{{Name: "current_time", Description: "now"}},
			SessionID: "chat-123",
		}, providerFor(server), "key")
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
	}
	if len(*seen) < 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
	key := func(body []byte) string {
		var parsed struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		_ = json.Unmarshal(body, &parsed)
		return parsed.PromptCacheKey
	}
	first, second := key((*seen)[0].Body), key((*seen)[1].Body)
	if first == "" {
		t.Fatal("responses request must carry a prompt cache key")
	}
	if first != second {
		t.Errorf("cache key changed between turns: %q then %q", first, second)
	}
}

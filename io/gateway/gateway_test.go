package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-engine/config"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
)

type memStore struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
	turns    map[string][]session.Turn
}

func newMemStore() *memStore {
	return &memStore{sessions: map[string]*session.Session{}, turns: map[string][]session.Turn{}}
}

func (m *memStore) Create(params session.NewSession) (*session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := &session.Session{
		ID:          "s1",
		Title:       params.Title,
		Provider:    params.Provider,
		Model:       params.Model,
		SubProvider: params.SubProvider,
		SubModel:    params.SubModel,
		SubEnabled:  params.SubEnabled,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   params.MaxTokens,
	}
	m.sessions[created.ID] = created
	return created, nil
}

func (m *memStore) Get(id string) (*session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found, ok := m.sessions[id]
	if !ok {
		return nil, io.EOF
	}
	return found, nil
}

func (m *memStore) List() ([]session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]session.Session, 0, len(m.sessions))
	for _, item := range m.sessions {
		out = append(out, *item)
	}
	return out, nil
}

func (m *memStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.turns, id)
	return nil
}

func (m *memStore) SaveConfig(id string, saved session.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.sessions[id]
	if !ok {
		return io.EOF
	}
	target.Provider = saved.Provider
	target.Model = saved.Model
	if saved.Title != "" {
		target.Title = saved.Title
	}
	return nil
}

func (m *memStore) Turns(id string) ([]session.Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]session.Turn(nil), m.turns[id]...), nil
}

func (m *memStore) History(id string) ([]provider.Message, error) {
	turns, err := m.Turns(id)
	if err != nil {
		return nil, err
	}
	return session.MessagesFromTurns(turns), nil
}

func (m *memStore) AppendTurns(id string, turns []session.TurnInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, turn := range turns {
		m.turns[id] = append(m.turns[id], session.Turn{
			SessionID:    id,
			Seq:          len(m.turns[id]) + 1,
			Role:         turn.Role,
			Text:         turn.Text,
			Model:        turn.Model,
			InputTokens:  turn.InputTokens,
			OutputTokens: turn.OutputTokens,
			DurationMS:   turn.DurationMS,
			GenerationMS: turn.GenerationMS,
		})
	}
	return nil
}

func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		request := struct {
			Stream bool `json:"stream"`
		}{}
		_ = json.Unmarshal(body, &request)

		answered := strings.Contains(string(body), `"tool_call_id"`)
		if request.Stream {
			if answered {
				streamFake(w, "done", nil, "stop")
			} else {
				streamFake(w, "", []string{"current_time"}, "tool_calls")
			}
			return
		}
		if answered {
			writeFake(w, "done", nil, "")
			return
		}
		writeFake(w, "", []string{"current_time"}, "thinking")
	}))
}

func streamFake(w http.ResponseWriter, content string, toolCalls []string, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if content != "" || len(toolCalls) > 0 {
		delta := map[string]any{"role": "assistant"}
		if content != "" {
			delta["content"] = content
		}
		if len(toolCalls) > 0 {
			calls := make([]map[string]any, 0, len(toolCalls))
			for _, name := range toolCalls {
				calls = append(calls, map[string]any{
					"id":       "call_1",
					"type":     "function",
					"index":    0,
					"function": map[string]any{"name": name, "arguments": "{}"},
				})
			}
			delta["tool_calls"] = calls
		}
		chunk := map[string]any{
			"id": "chatcmpl-fake",
			"choices": []map[string]any{
				{"index": 0, "delta": delta, "finish_reason": nil},
			},
		}
		encoded, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
	}
	final := map[string]any{
		"id": "chatcmpl-fake",
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
		},
	}
	encoded, _ := json.Marshal(final)
	_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func writeFake(w http.ResponseWriter, content string, toolCalls []string, finish string) {
	w.Header().Set("Content-Type", "application/json")
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(toolCalls))
		for i, name := range toolCalls {
			calls = append(calls, map[string]any{
				"id":       "call_1",
				"type":     "function",
				"index":    i,
				"function": map[string]any{"name": name, "arguments": "{}"},
			})
		}
		message["tool_calls"] = calls
		finish = "tool_calls"
	}
	payload := map[string]any{
		"id":      "chatcmpl-fake",
		"model":   "test-model",
		"choices": []map[string]any{{"message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func newTestGateway(t *testing.T, store SessionStore) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := fakeUpstream(t)
	t.Cleanup(upstream.Close)
	config.Providers = []config.Provider{{
		ID:      "9",
		Name:    "mock",
		APIURL:  upstream.URL,
		Keys:    []string{"test-key"},
		Adapter: "openai",
	}}
	return New(store, registry.New(), tools.Builtin()), upstream
}

func postJSON(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestChatCompletionRunsToolAndReturnsAnswer(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"what time is it"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Provider string `json:"provider"`
		Choices  []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Provider != "mock" {
		t.Errorf("provider = %q, want mock", payload.Provider)
	}
	if got := payload.Choices[0].Message.Content; got != "done" {
		t.Errorf("content = %q, want done", got)
	}
	if got := payload.Usage.TotalTokens; got != 14 {
		t.Errorf("total tokens = %d, want 14 (two rounds)", got)
	}
}

func TestChatCompletionStreams(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("missing chunk objects:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]:\n%s", body)
	}
	if strings.Count(body, "\n\n") < 3 {
		t.Errorf("expected several SSE events:\n%s", body)
	}
}

func TestChatRejectsUnknownModel(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "prefix it with a provider") {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

func TestChatWithSessionPersistsTurns(t *testing.T) {
	store := newMemStore()
	gateway, _ := newTestGateway(t, store)

	created := postJSON(t, gateway.Handler(), "/v1/sessions", `{"title":"t","model":"mock/test-model"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"session_id":"s1","messages":[{"role":"user","content":"hello"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	turns, _ := store.Turns("s1")
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4 (user, tool_call, tool_result, assistant)", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text != "hello" {
		t.Errorf("first turn = %+v", turns[0])
	}
	if turns[1].Role != "tool_call" || !strings.Contains(turns[1].Text, "current_time") {
		t.Errorf("tool call turn = %+v", turns[1])
	}
	if turns[2].Role != "tool_result" {
		t.Errorf("tool result turn = %+v", turns[2])
	}
	if turns[3].Role != "assistant" || turns[3].Text != "done" {
		t.Errorf("last turn = %+v", turns[3])
	}
	if store.sessions["s1"].Model != "test-model" {
		t.Errorf("session model = %q, want test-model", store.sessions["s1"].Model)
	}
}

func TestMergeHistoryAppendsOnlyNewMessages(t *testing.T) {
	stored := []provider.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
	}
	incoming := []provider.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	}

	full, delta := mergeHistory(stored, incoming)
	if len(full) != 3 {
		t.Errorf("full = %d messages, want 3", len(full))
	}
	if len(delta) != 1 || delta[0].Content != "three" {
		t.Errorf("delta = %+v, want only the new message", delta)
	}
}

func TestToolStepsReplayOnNextTurn(t *testing.T) {
	store := newMemStore()
	gateway, _ := newTestGateway(t, store)
	postJSON(t, gateway.Handler(), "/v1/sessions", `{"title":"t","model":"mock/test-model"}`)

	postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"session_id":"s1","messages":[{"role":"user","content":"hello"}]}`)
	// The client only knows visible text, so it sends the answer back without tool steps.
	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"session_id":"s1","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	turns, _ := store.Turns("s1")
	if len(turns) != 6 {
		t.Fatalf("turns = %d, want 6 (second turn adds only user + assistant)", len(turns))
	}

	history, _ := store.History("s1")
	var sawToolCall, sawToolResult bool
	for _, message := range history {
		if len(message.ToolCalls) > 0 {
			sawToolCall = true
		}
		if message.Role == "tool" && message.ToolCallID == "call_1" {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("history lost tool steps: %+v", history)
	}
}

func TestMergeHistoryKeepsToolStepsBetweenVisibleMessages(t *testing.T) {
	stored := []provider.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c", Name: "current_time", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c", Content: "10:00"},
		{Role: "assistant", Content: "it is 10:00"},
	}
	incoming := []provider.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "it is 10:00"},
		{Role: "user", Content: "thanks"},
	}

	full, delta := mergeHistory(stored, incoming)
	if len(full) != 5 {
		t.Fatalf("full = %d messages, want 5 (stored steps kept)", len(full))
	}
	if len(delta) != 1 || delta[0].Content != "thanks" {
		t.Errorf("delta = %+v", delta)
	}
}

func TestCircuitBreakerPausesThenRecovers(t *testing.T) {
	gateway := New(newMemStore(), registry.New(), tools.Builtin())
	now := time.Now()
	outage := &provider.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"}

	for i := 0; i < breakerThreshold; i++ {
		gateway.recordOutcome("mock", outage, now)
	}
	if !gateway.circuitOpen("mock", now.Add(time.Second)) {
		t.Fatal("breaker should be open after repeated outages")
	}
	if gateway.circuitOpen("mock", now.Add(breakerCooldown+time.Second)) {
		t.Fatal("breaker should close after the cooldown")
	}

	gateway.recordOutcome("mock", outage, now)
	gateway.recordOutcome("mock", nil, now)
	gateway.recordOutcome("mock", outage, now)
	gateway.recordOutcome("mock", outage, now)
	if gateway.circuitOpen("mock", now) {
		t.Error("a success should reset the failure count")
	}
}

func TestCircuitBreakerIgnoresClientErrors(t *testing.T) {
	gateway := New(newMemStore(), registry.New(), tools.Builtin())
	now := time.Now()
	badKey := &provider.APIError{StatusCode: http.StatusBadRequest, Message: "bad request"}
	for i := 0; i < breakerThreshold+2; i++ {
		gateway.recordOutcome("mock", badKey, now)
	}
	if gateway.circuitOpen("mock", now) {
		t.Error("400 responses must not pause the provider")
	}
}

func TestMergeHistoryWithNoCommonPrefixKeepsStoredSteps(t *testing.T) {
	stored := []provider.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c", Name: "current_time", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c", Content: "10:00"},
		{Role: "assistant", Content: "it is 10:00"},
	}
	incoming := []provider.Message{{Role: "user", Content: "a different first message"}}

	full, delta := mergeHistory(stored, incoming)
	if len(full) != 1 || full[0].Content != "a different first message" {
		t.Errorf("full = %+v, want only the incoming message when nothing matches", full)
	}
	if len(delta) != 1 {
		t.Errorf("delta = %+v", delta)
	}
}

// When the model keeps asking for tools past the round limit, it must still
// answer: the gateway asks once more without tools.
func TestToolRoundLimitStillGivesAnAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools"`) {
			writeFake(w, "", []string{"current_time"}, "tool_calls")
			return
		}
		writeFake(w, "here is the answer", nil, "stop")
	}))
	defer upstream.Close()
	config.Providers = []config.Provider{{
		ID: "9", Name: "mock", APIURL: upstream.URL, Keys: []string{"test-key"}, Adapter: "openai",
	}}
	gateway := New(newMemStore(), registry.New(), tools.Builtin())

	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"what time"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "here is the answer") {
		t.Errorf("reply had no answer after the tool limit: %s", recorder.Body.String())
	}
}

func TestRejectsInvalidUTF8Body(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())
	bad := "{\"model\":\"mock/test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"\xe0\xb8\"}]}"
	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions", bad)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid UTF-8", recorder.Code)
	}
}

func TestRejectsOversizedRequestBody(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())
	body := strings.Repeat(" ", requestMaxBody+1)
	recorder := postJSON(t, gateway.Handler(), "/v1/chat/completions", body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "exceeds the") {
		t.Fatalf("body = %s, want request-size error", recorder.Body.String())
	}
}

func TestIsRepeatOfTail(t *testing.T) {
	user := func(text string) provider.Message { return provider.Message{Role: "user", Content: text} }
	asst := func(text string) provider.Message { return provider.Message{Role: "assistant", Content: text} }
	if !isRepeatOfTail([]provider.Message{user("hi")}, []provider.Message{user("hi")}) {
		t.Error("identical trailing user turn should count as repeat")
	}
	if isRepeatOfTail([]provider.Message{user("hi")}, []provider.Message{user("other")}) {
		t.Error("different text must not count as repeat")
	}
	if isRepeatOfTail([]provider.Message{user("hi"), asst("yo")}, []provider.Message{user("hi")}) {
		t.Error("answered turn must not count as repeat")
	}
	if isRepeatOfTail(nil, []provider.Message{user("hi")}) {
		t.Error("empty history must not count as repeat")
	}
}

func TestToolHooksFireDuringRounds(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())
	impl, err := gateway.registry.Require("openai")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Providers[0]
	primary := route{adapter: impl, prov: toInternalProvider(cfg), model: "test-model"}
	defs := []provider.ToolDef{tools.Builtin()["current_time"].Def}
	var calls []string
	var results []string
	options := completionOptions{
		maxTokens: 50,
		tools:     defs,
		onToolCall: func(call provider.ToolCall) {
			calls = append(calls, call.Name)
		},
		onToolResult: func(call provider.ToolCall, text string, durationMs int64, isError bool) {
			results = append(results, call.Name)
		},
	}
	resp, err := gateway.complete(context.Background(), primary, nil, "",
		[]provider.Message{{Role: "user", Content: "what time is it"}}, options)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Content != "done" {
		t.Errorf("content = %q, want done", resp.Content)
	}
	if len(calls) != 1 || calls[0] != "current_time" {
		t.Errorf("tool calls = %v, want [current_time]", calls)
	}
	if len(results) != 1 || results[0] != "current_time" {
		t.Errorf("tool results = %v, want [current_time]", results)
	}
}

func TestUISpecMerge(t *testing.T) {
	gateway, _ := newTestGateway(t, newMemStore())
	spec, err := gateway.uiSpec()
	if err != nil {
		t.Fatalf("uiSpec: %v", err)
	}
	var base map[string]any
	if err := json.Unmarshal(spec, &base); err != nil {
		t.Fatalf("spec not json: %v", err)
	}
	strings, _ := base["strings"].(map[string]any)
	if strings["retry"] != "ลองอีกครั้ง" {
		t.Errorf("default retry = %v", strings["retry"])
	}
	// A partial override changes only the keys it names.
	dst := map[string]any{"strings": map[string]any{"retry": "กดซ้ำ", "dismiss": "ปิด"}}
	mergeMaps(dst, map[string]any{"strings": map[string]any{"retry": "ใหม่"}})
	merged := dst["strings"].(map[string]any)
	if merged["retry"] != "ใหม่" {
		t.Errorf("retry = %v, want ใหม่", merged["retry"])
	}
	if merged["dismiss"] != "ปิด" {
		t.Errorf("dismiss was dropped: %v", merged["dismiss"])
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"/abs/photo.jpg":   "photo.jpg",
		"my file (1).png":  "my file (1).png",
		"...":              "",
		"รูปถ่าย.jpg":      "รูปถ่าย.jpg",
	}
	for in, want := range cases {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeSegment("ses-1/../../x"); got != "ses-1x" {
		t.Errorf("safeSegment = %q", got)
	}
}

func TestAppendDeltaKeepsStoredHistory(t *testing.T) {
	user := func(s string) provider.Message { return provider.Message{Role: "user", Content: s} }
	asst := func(s string) provider.Message { return provider.Message{Role: "assistant", Content: s} }
	stored := []provider.Message{user("คำถามแรก"), asst("คำตอบแรก"), user("คำถามที่สอง"), asst("คำตอบที่สอง")}
	incoming := []provider.Message{user("คำถามที่สาม")}

	// What the WebSocket path used to do: a single new message shares no
	// prefix with the stored transcript, so everything before it was cut and
	// the model answered as if this were a fresh conversation.
	if _, full := mergeHistory(stored, incoming); len(full) != 1 {
		t.Fatalf("mergeHistory is expected to drop history for a single new turn; got %d messages", len(full))
	}

	full, delta := appendDelta(stored, incoming)
	if len(full) != 5 {
		t.Fatalf("appendDelta kept %d messages, want the whole conversation (5)", len(full))
	}
	if full[0].Content != "คำถามแรก" {
		t.Errorf("first message = %q, want คำถามแรก", full[0].Content)
	}
	if len(delta) != 1 || delta[0].Content != "คำถามที่สาม" {
		t.Errorf("delta = %+v, want only the new turn", delta)
	}
}

func TestAppendDeltaReusesRecordedQuestionOnRetry(t *testing.T) {
	user := func(s string) provider.Message { return provider.Message{Role: "user", Content: s} }
	// The failed attempt already recorded the question; a retry must not
	// stack a twin the model would read as the user asking twice.
	stored := []provider.Message{user("ก่อนหน้า"), user("คำถามที่ล้มแล้ว")}
	full, delta := appendDelta(stored, []provider.Message{user("คำถามที่ล้มแล้ว")})
	if len(full) != 2 {
		t.Errorf("retry stacked a twin: %d messages, want 2", len(full))
	}
	if len(delta) != 0 {
		t.Errorf("retry should persist nothing new, got %d messages", len(delta))
	}
}

func TestFinalAnswerHoldsOnlyTheLastToolRound(t *testing.T) {
	// A model that narrates before each tool call. Accumulating those rounds into
	// the final message produced one run-on answer: "กำลังดูครับคำตอบสุดท้าย".
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tool_call_id"`) {
			writeFake(w, "คำตอบสุดท้าย", nil, "")
			return
		}
		writeFake(w, "กำลังดูครับ", []string{"current_time"}, "tool_calls")
	}))
	t.Cleanup(upstream.Close)
	config.Providers = []config.Provider{{
		ID: "9", Name: "mock", APIURL: upstream.URL,
		Keys: []string{"test-key"}, Adapter: "openai",
	}}
	gateway := New(newMemStore(), registry.New(), tools.Builtin())

	impl, err := gateway.registry.Require("openai")
	if err != nil {
		t.Fatal(err)
	}
	primary := route{adapter: impl, prov: toInternalProvider(config.Providers[0]), model: "test-model"}
	defs := []provider.ToolDef{tools.Builtin()["current_time"].Def}

	resp, err := gateway.complete(context.Background(), primary, nil, "",
		[]provider.Message{{Role: "user", Content: "เวลาเท่าไหร่"}},
		completionOptions{maxTokens: 100, tools: defs})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Content != "คำตอบสุดท้าย" {
		t.Errorf("answer = %q, want only the last round's text", resp.Content)
	}
}

func TestGenerationMillisCountsWritingNotWaiting(t *testing.T) {
	// A turn that reached a provider reports how long the model spent writing,
	// which is what the elapsed figure and the tokens-per-second both mean.
	if got := generationMillis(time.Now().Add(-time.Hour), &provider.Response{GenerationMS: 4200}); got != 4200 {
		t.Errorf("generation = %d, want 4200 (not the wall time)", got)
	}
	// A turn that never reached a provider still reports something sensible
	// rather than "0s".
	if got := generationMillis(time.Now().Add(-150*time.Millisecond), &provider.Response{}); got < 140 {
		t.Errorf("fallback = %d, want roughly the wall time", got)
	}
}

// The footer shows two different numbers from one turn: how long the reader
// waited (the whole turn) and how fast the model wrote (the provider window).
// Collapsing them into one column made either the elapsed time or the rate
// wrong, so both are stored and both are checked here.
func TestTurnStoresBothTheWaitAndTheWrite(t *testing.T) {
	store := &memStore{turns: map[string][]session.Turn{}}
	g := &Gateway{sessions: store}

	resp := &provider.Response{
		Content:      "ตอบแล้ว",
		Model:        "muse-spark",
		GenerationMS: 4200,
		Usage:        provider.Usage{PromptTokens: 10, CompletionTokens: 84},
	}
	g.persist(&session.Session{ID: "s1"}, []provider.Message{{Role: "user", Content: "ถาม"}}, resp, completionOptions{}, 9000)

	if len(store.turns["s1"]) != 2 {
		t.Fatalf("want the question and the answer stored, got %d turns", len(store.turns["s1"]))
	}
	got := store.turns["s1"][1]
	if got.DurationMS != 9000 {
		t.Errorf("wait = %dms, want the whole turn 9000ms", got.DurationMS)
	}
	if got.GenerationMS != 4200 {
		t.Errorf("write = %dms, want the provider window 4200ms", got.GenerationMS)
	}
}

// A provider that reports nothing usable must not produce a rate window longer
// than the turn itself, which would divide the token count by a time that never
// happened.
func TestGenerationFallsBackToTheTurn(t *testing.T) {
	store := &memStore{turns: map[string][]session.Turn{}}
	g := &Gateway{sessions: store}

	resp := &provider.Response{Content: "ตอบแล้ว", GenerationMS: 60000, Usage: provider.Usage{CompletionTokens: 10}}
	g.persist(&session.Session{ID: "s2"}, []provider.Message{{Role: "user", Content: "ถาม"}}, resp, completionOptions{}, 2500)

	if got := store.turns["s2"][1].GenerationMS; got != 2500 {
		t.Errorf("write = %dms, want the turn 2500ms rather than the impossible 60000ms", got)
	}
}

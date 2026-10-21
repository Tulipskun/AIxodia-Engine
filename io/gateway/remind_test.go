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

	"aixodia-engine/config"
	"aixodia-engine/goals"
	"aixodia-engine/provider"
	"aixodia-engine/provider/registry"
	"aixodia-engine/session"
	"aixodia-engine/tools"
)

// memObjectives is a small in-memory goals.Store for reminder tests. The tool
// tests live in the tools package; this fake only has to answer Open.
type memObjectives struct {
	items map[string][]goals.Item
	err   error
}

func (m *memObjectives) Add(string, goals.Kind, string) (string, error) { return "", nil }
func (m *memObjectives) Edit(string, goals.Kind, string, string) error  { return nil }
func (m *memObjectives) Complete(string, goals.Kind, string) error      { return nil }
func (m *memObjectives) Drop(string, goals.Kind, string) error          { return nil }
func (m *memObjectives) Open(sessionID string) ([]goals.Item, error) {
	if m.err != nil {
		return nil, m.err
	}
	return append([]goals.Item(nil), m.items[sessionID]...), nil
}

func reminderMessages(messages []provider.Message) []provider.Message {
	var out []provider.Message
	for _, message := range messages {
		if isReminder(message) {
			out = append(out, message)
		}
	}
	return out
}

func TestPrepareMessagesInjectsActiveObjectivesAtTheTail(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {
			{ID: "g-1111", Kind: goals.Goal, Content: "ship the release"},
			{ID: "p-2222", Kind: goals.Plan, Content: "write the tests"},
		},
	}}
	g := &Gateway{objectives: store}
	state := &reminderState{}
	conversation, messages, whole := g.prepareMessages(
		[]provider.Message{{Role: "user", Content: "what is next?"}},
		session.Window{Max: 100000}, "chat-1", state)

	if !whole {
		t.Error("whole = false for a conversation that fits")
	}
	if len(conversation) != 2 || conversation[0].Content != "what is next?" || !isReminder(conversation[1]) {
		t.Fatalf("conversation = %#v, want the question then the reminder", conversation)
	}
	if !hasReminder(messages) {
		t.Fatal("messages have no reminder")
	}
	if !isReminder(messages[len(messages)-1]) {
		t.Fatalf("reminder is not the last message: %#v", messages)
	}
	text := messages[len(messages)-1].Content
	for _, want := range []string{
		"<system-remind>", "</system-remind>",
		"Goals:\n- [g-1111] ship the release",
		"Plans:\n- [p-2222] write the tests",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reminder missing %q:\n%s", want, text)
		}
	}
}

func TestPrepareMessagesKeepsAUserAttachmentWithTheReminder(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {{ID: "g-1111", Kind: goals.Goal, Content: "look at the picture"}},
	}}
	g := &Gateway{objectives: store}
	conversation, _, _ := g.prepareMessages([]provider.Message{{
		Role: "user", Content: "what is this?", Images: []provider.Image{{}},
	}}, session.Window{Max: 100000}, "chat-1", &reminderState{})

	if len(conversation) != 2 || len(conversation[0].Images) != 1 || !isReminder(conversation[1]) {
		t.Fatalf("conversation = %#v, want the attached question then the reminder", conversation)
	}
}

func TestPrepareMessagesSkipsEmptyStoreUntilSomethingChanged(t *testing.T) {
	g := &Gateway{objectives: &memObjectives{items: map[string][]goals.Item{}}}
	state := &reminderState{}
	conversation, messages, _ := g.prepareMessages(
		[]provider.Message{{Role: "user", Content: "hello"}},
		session.Window{Max: 100000}, "chat-1", state)

	if hasReminder(conversation) || hasReminder(messages) {
		t.Error("an empty store with no change injected a reminder")
	}
	if !state.checkedEmpty {
		t.Error("checkedEmpty = false after an empty lookup")
	}
}

func TestPrepareMessagesInjectsEmptyListAfterAChange(t *testing.T) {
	g := &Gateway{objectives: &memObjectives{items: map[string][]goals.Item{}}}
	state := &reminderState{dirty: true}
	_, messages, _ := g.prepareMessages(
		[]provider.Message{{Role: "user", Content: "done"}},
		session.Window{Max: 100000}, "chat-1", state)

	if !hasReminder(messages) {
		t.Fatal("a completed last item did not produce a reminder")
	}
	text := messages[len(messages)-1].Content
	if !strings.Contains(text, "Goals:\n- none") {
		t.Errorf("empty reminder does not say none:\n%s", text)
	}
	if state.dirty {
		t.Error("dirty was not cleared after injecting")
	}
}

func TestPrepareMessagesReplacesAStaleReminder(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {{ID: "g-1111", Kind: goals.Goal, Content: "new content"}},
	}}
	g := &Gateway{objectives: store}
	state := &reminderState{dirty: true}
	conversation, messages, _ := g.prepareMessages([]provider.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "<system-remind>\nold list\n</system-remind>"},
		{Role: "assistant", Content: "working"},
		{Role: "tool", ToolName: "goal_edit", Content: "goal g-1111 updated"},
	}, session.Window{Max: 100000}, "chat-1", state)

	reminders := reminderMessages(messages)
	if len(reminders) != 1 {
		t.Fatalf("reminders = %d, want exactly one", len(reminders))
	}
	if strings.Contains(reminders[0].Content, "old list") || !strings.Contains(reminders[0].Content, "new content") {
		t.Errorf("reminder was not refreshed:\n%s", reminders[0].Content)
	}
	if hasReminder(conversation[:len(conversation)-1]) {
		t.Error("the old reminder is still in the conversation")
	}
}

func TestPrepareMessagesPinsTheReminderAfterTrim(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {{ID: "g-1111", Kind: goals.Goal, Content: "keep this"}},
	}}
	g := &Gateway{objectives: store}
	state := &reminderState{}
	conversation := []provider.Message{
		{Role: "user", Content: "a long time ago"},
		{Role: "assistant", Content: strings.Repeat("old ", 500)},
		{Role: "user", Content: "<system-remind>\nstale\n</system-remind>"},
		{Role: "assistant", Content: strings.Repeat("more ", 500)},
		{Role: "user", Content: "the new question"},
	}

	_, messages, whole := g.prepareMessages(conversation, session.Window{Max: 400}, "chat-1", state)
	if whole {
		t.Fatal("whole = true, expected the old history to be trimmed")
	}
	reminders := reminderMessages(messages)
	if len(reminders) != 1 || !strings.Contains(reminders[0].Content, "keep this") {
		t.Fatalf("reminders = %#v, want one fresh reminder", reminders)
	}
	if messages[len(messages)-2].Content != "the new question" {
		t.Errorf("question = %q, want it immediately before the reminder", messages[len(messages)-2].Content)
	}
	if !isReminder(messages[len(messages)-1]) {
		t.Errorf("reminder is not the last message: %#v", messages)
	}
}

func TestPrepareMessagesKeepsOneReminderAtTheTailAcrossRounds(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {{ID: "g-1111", Kind: goals.Goal, Content: "stay focused"}},
	}}
	g := &Gateway{objectives: store}
	state := &reminderState{}
	conversation := []provider.Message{{Role: "user", Content: "start"}}

	conversation, _, _ = g.prepareMessages(conversation, session.Window{Max: 100000}, "chat-1", state)
	conversation = append(conversation,
		provider.Message{Role: "assistant", Content: "working"},
		provider.Message{Role: "tool", ToolName: "goal_edit", Content: "goal g-1111 updated"},
	)
	_, messages, _ := g.prepareMessages(conversation, session.Window{Max: 100000}, "chat-1", state)

	reminders := reminderMessages(messages)
	if len(reminders) != 1 {
		t.Fatalf("reminders = %d, want exactly one", len(reminders))
	}
	if !isReminder(messages[len(messages)-1]) {
		t.Fatalf("reminder is not the last message: %#v", messages)
	}
	if !strings.Contains(reminders[0].Content, "stay focused") {
		t.Errorf("reminder lost the active goal:\n%s", reminders[0].Content)
	}
}

func TestPrepareMessagesDoesNotLeakAnotherSessionsObjectives(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{
		"chat-1": {{ID: "g-1111", Kind: goals.Goal, Content: "only for chat 1"}},
	}}
	g := &Gateway{objectives: store}
	state := &reminderState{}
	_, messages, _ := g.prepareMessages(
		[]provider.Message{{Role: "user", Content: "hi"}},
		session.Window{Max: 100000}, "chat-2", state)

	if hasReminder(messages) {
		t.Errorf("chat-2 saw chat-1's reminder:\n%#v", messages)
	}
}

// mutableObjectives is a store that actually records an Add, so the full tool
// loop can be tested: the tool writes, the gateway notices, and the next round
// carries the new list.
type mutableObjectives struct {
	mu    sync.Mutex
	items map[string][]goals.Item
}

func (m *mutableObjectives) Add(sessionID string, kind goals.Kind, content string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "g-1"
	if kind == goals.Plan {
		id = "p-1"
	}
	if m.items == nil {
		m.items = map[string][]goals.Item{}
	}
	m.items[sessionID] = append(m.items[sessionID], goals.Item{ID: id, Kind: kind, Content: content})
	return id, nil
}

func (m *mutableObjectives) Edit(string, goals.Kind, string, string) error { return nil }
func (m *mutableObjectives) Complete(string, goals.Kind, string) error     { return nil }
func (m *mutableObjectives) Drop(string, goals.Kind, string) error         { return nil }

func (m *mutableObjectives) Open(sessionID string) ([]goals.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]goals.Item(nil), m.items[sessionID]...), nil
}

func TestCompleteInjectsAReminderAfterAGoalToolUpdate(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if strings.Contains(string(body), `"tool_call_id"`) {
			writeFake(w, "done", nil, "")
			return
		}
		// First round asks for goal_add with real arguments instead of the
		// empty "{}" helper shape.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-goal",
			"model": "test-model",
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{{
						"id":       "call_goal",
						"type":     "function",
						"function": map[string]any{"name": "goal_add", "arguments": `{"content":"test goal"}`},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	}))
	defer upstream.Close()

	config.Providers = []config.Provider{{
		ID:      "9",
		Name:    "mock",
		APIURL:  upstream.URL,
		Keys:    []string{"test-key"},
		Adapter: "openai",
	}}
	objectives := &mutableObjectives{}
	g := New(newMemStore(), registry.New(), tools.BuiltinWith(objectives))
	g.SetObjectiveStore(objectives)
	impl, err := g.registry.Require("openai")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Providers[0]

	resp, err := g.complete(context.Background(),
		route{adapter: impl, prov: toInternalProvider(cfg), model: "test-model"}, nil,
		[]provider.Message{{Role: "user", Content: "make a goal"}},
		completionOptions{
			sessionID: "chat-1",
			maxTokens: 100,
			tools:     []provider.ToolDef{g.tools["goal_add"].Def},
		})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("content = %q, want done", resp.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "system-remind") || !strings.Contains(bodies[1], "test goal") {
		t.Errorf("second request has no fresh reminder:\n%s", bodies[1])
	}
	items, _ := objectives.Open("chat-1")
	if len(items) != 1 || items[0].Content != "test goal" {
		t.Errorf("store items = %#v, want one goal", items)
	}
}

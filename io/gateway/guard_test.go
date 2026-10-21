package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aixodia-engine/config"
	"aixodia-engine/goals"
	"aixodia-engine/provider"
	"aixodia-engine/provider/registry"
	"aixodia-engine/tools"
)

func TestProviderRequestStartsWithAIxodiaPrompt(t *testing.T) {
	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		writeFake(w, "done", nil, "")
	}))
	t.Cleanup(upstream.Close)
	config.Providers = []config.Provider{{
		ID:      "9",
		Name:    "mock",
		APIURL:  upstream.URL,
		Keys:    []string{"test-key"},
		Adapter: "openai",
	}}
	g := New(newMemStore(), registry.New(), tools.Builtin())
	rec := postJSON(t, g.Handler(), "/v1/chat/completions",
		`{"model":"mock/test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Messages []provider.Message `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(body.Messages) == 0 || body.Messages[0].Role != "system" {
		t.Fatalf("first message = %+v, want the AIxodia system prompt", body.Messages)
	}
	for _, want := range []string{
		"You are AIxodia",
		`Never run the "aixodia" command`,
		"Never kill, terminate, restart, or signal yourself",
		"active goal or plan",
	} {
		if !strings.Contains(body.Messages[0].Content, want) {
			t.Errorf("system prompt does not mention %q", want)
		}
	}
}

func TestAIxodiaSystemPromptIsEnglishOnly(t *testing.T) {
	for _, r := range aixodiaSystemPrompt {
		if r > 127 {
			t.Fatalf("system prompt contains non-ASCII character %q; keep it English-only", r)
		}
	}
}

func TestOrdinaryToolRefusedWithoutActiveObjective(t *testing.T) {
	store := &memObjectives{items: map[string][]goals.Item{}}
	g := &Gateway{tools: tools.BuiltinWith(store), objectives: store}
	ctx := tools.WithSession(context.Background(), "s1")

	out, _ := g.runTool(ctx, provider.ToolCall{Name: "current_time"}, "s1")
	if !isToolError(out) || !strings.Contains(out, "goal_add") {
		t.Fatalf("without a goal, current_time ran: %s", out)
	}

	// Goal tools stay available so the model can create the missing objective.
	out, _ = g.runTool(ctx, provider.ToolCall{
		Name: "goal_add", Arguments: `{"content":"finish the task"}`,
	}, "s1")
	if isToolError(out) {
		t.Fatalf("goal_add was refused without a goal: %s", out)
	}

	store.items["s1"] = []goals.Item{{ID: "g1", Kind: goals.Goal, Content: "finish the task"}}
	out, _ = g.runTool(ctx, provider.ToolCall{Name: "current_time"}, "s1")
	if isToolError(out) {
		t.Fatalf("current_time still refused with an active goal: %s", out)
	}
}

func TestBeginTurnRejectsASecondTurnOnOneChat(t *testing.T) {
	g := &Gateway{turns: map[string]bool{}}
	if !g.beginTurn("s1") {
		t.Fatal("first turn was refused")
	}
	if g.beginTurn("s1") {
		t.Fatal("second concurrent turn was allowed")
	}
	if !g.beginTurn("s2") {
		t.Fatal("a turn on another chat was refused")
	}
	g.endTurn("s1")
	if !g.beginTurn("s1") {
		t.Fatal("chat stayed locked after its turn ended")
	}
}

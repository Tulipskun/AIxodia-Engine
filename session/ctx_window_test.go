package session

import (
	"strings"
	"testing"

	"aixodia-engine/provider"
)

// sized builds a message whose estimated token count is easy to reason about:
// a run of ASCII letters prices at four characters a token, plus 4 for the
// message itself.
func sized(role string, chars int) provider.Message {
	return provider.Message{Role: role, Content: strings.Repeat("x", chars)}
}

// callOf builds the assistant message that makes a tool call.
func callOf(id string) provider.Message {
	return provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
		{ID: id, Name: "shell", Arguments: "{}"},
	}}
}

// resultOf builds a tool result.
func resultOf(id string) provider.Message {
	return provider.Message{Role: "tool", ToolCallID: id, ToolName: "shell", Content: "ok"}
}

// checkWindow fails when a trimmed history is one the upstream would refuse:
// it may not open on an assistant or tool message, and every tool result must
// still have the call it answers ahead of it.
func checkWindow(t *testing.T, messages []provider.Message) {
	t.Helper()
	for _, message := range messages {
		if message.Role == "system" {
			continue
		}
		if message.Role != "user" {
			t.Fatalf("window opens on %q, want a user message: %+v", message.Role, messages)
		}
		break
	}
	answered := map[string]bool{}
	for i, message := range messages {
		for _, call := range message.ToolCalls {
			answered[call.ID] = true
		}
		if message.Role == "tool" && !answered[message.ToolCallID] {
			t.Fatalf("tool result %q at %d has no call ahead of it: %+v", message.ToolCallID, i, messages)
		}
	}
}

func TestEstimateTokensPricesEveryKindOfText(t *testing.T) {
	cases := []struct {
		name string
		text string
		low  int
		high int
	}{
		{"empty", "", 0, 0},
		{"short word", "hi", 1, 1},
		{"english words", "the quick brown fox jumps over the lazy dog", 8, 12},
		{"digits", "12345678", 2, 3},
		{"braces", `{"a":1,"b":2}`, 6, 10},
		{"thai", "สวัสดีครับ", 5, 8},
		{"cjk", "你好世界", 4, 4},
		{"emoji", "🙂🙂", 1, 3},
	}
	for _, tc := range cases {
		got := EstimateTokens(tc.text)
		if got < tc.low || got > tc.high {
			t.Errorf("EstimateTokens(%q) = %d, want %d..%d", tc.text, got, tc.low, tc.high)
		}
	}
	// The old count charged Thai four characters a token; the point of the new
	// one is that the same number of characters can cost differently.
	if EstimateTokens(strings.Repeat("x", 40)) == EstimateTokens(strings.Repeat("ก", 40)) {
		t.Error("Latin and Thai of equal length priced the same; scripts are not equal to a tokenizer")
	}
}

func TestWindowSizeAnchorsOnTheProviderCount(t *testing.T) {
	messages := []provider.Message{sized("system", 40), sized("user", 40), sized("assistant", 40)}
	window := Window{Max: 1000, Usage: 50, Messages: 2}
	if got, want := window.Size(messages), 50+Tokens(messages[2:]); got != want {
		t.Errorf("anchored size = %d, want %d", got, want)
	}
	// A count that reaches past the conversation cannot describe it, so the
	// estimate has to answer instead.
	stale := Window{Max: 1000, Usage: 50, Messages: 9}
	if got, want := stale.Size(messages), Tokens(messages); got != want {
		t.Errorf("stale size = %d, want the full estimate %d", got, want)
	}
	if got, want := (Window{Max: 1000}).Size(messages), Tokens(messages); got != want {
		t.Errorf("empty size = %d, want the full estimate %d", got, want)
	}
}

func TestTrimKeepsEverythingUnderBudget(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 40),
		sized("assistant", 40),
	}
	got := Trim(messages, Window{Max: 100000})
	if len(got) != len(messages) {
		t.Fatalf("got %d messages, want the whole conversation: %+v", len(got), got)
	}
	for i := range messages {
		if got[i].Role != messages[i].Role {
			t.Errorf("message %d = %q, want %q", i, got[i].Role, messages[i].Role)
		}
	}
}

// The provider's own count settles the fit even when the estimate would have
// trimmed: the count measured the prompt, the estimate guessed at it.
func TestTrimTrustsTheProviderCount(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 4000),
		sized("assistant", 4000),
	}
	// The estimate says the conversation is far over budget; the provider's
	// count says it is a fifth of it.
	window := Window{Max: 100000, Usage: 500, Messages: len(messages)}
	got := Trim(messages, window)
	if len(got) != len(messages) {
		t.Fatalf("got %d messages, want the count to keep them all: %+v", len(got), got)
	}
}

// The window slides from the newest side: the oldest turn goes first, and the
// turn being asked stays.
func TestTrimDropsTheOldestTurn(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 400),
		sized("assistant", 400),
		sized("user", 400),
		sized("assistant", 400),
		sized("user", 400),
		sized("assistant", 400),
	}
	turn := Tokens(messages[1:3])
	// Two turns fit beside the system block; the third does not.
	window := Window{Max: Tokens(messages[:1]) + 2*turn + 1}
	got := Trim(messages, window)
	want := []provider.Message{messages[0], messages[3], messages[4], messages[5], messages[6]}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Fatalf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A tool call and the results answering it are one turn. A budget that cannot
// hold the whole turn drops all of it, instead of leaving a window that opens
// on an assistant or on an orphaned result.
func TestTrimNeverSplitsAToolTurn(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 400),
		callOf("c1"),
		resultOf("c1"),
		sized("user", 400),
		sized("assistant", 400),
	}
	newest := Tokens(messages[4:])
	window := Window{Max: Tokens(messages[:1]) + newest + 1}
	got := Trim(messages, window)
	want := []provider.Message{messages[0], messages[4], messages[5]}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Role != want[i].Role {
			t.Fatalf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	checkWindow(t, got)
}

// The newest turn is kept even when it alone is over budget: trimming it away
// would send a request with nothing to answer.
func TestTrimAlwaysKeepsTheNewestTurn(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 400),
		sized("assistant", 400),
		sized("user", 400),
		sized("assistant", 400),
	}
	got := Trim(messages, Window{Max: Tokens(messages[:1]) + 1})
	if len(got) != 3 {
		t.Fatalf("got %d messages, want the system block plus the newest turn: %+v", len(got), got)
	}
	if got[1].Role != "user" || got[2].Role != "assistant" {
		t.Fatalf("newest turn = %+v, want a user and its assistant", got[1:])
	}
}

// Over a sweep of budgets the window is always something the upstream accepts,
// and it never drops the message the model was asked to answer.
func TestTrimWindowIsAlwaysSendable(t *testing.T) {
	messages := []provider.Message{
		sized("system", 200),
		sized("user", 400),
		sized("assistant", 400),
		sized("user", 400),
		callOf("a"),
		resultOf("a"),
		sized("assistant", 400),
		sized("user", 400),
		callOf("b"),
		resultOf("b"),
		sized("assistant", 400),
		sized("user", 400),
		sized("assistant", 400),
	}
	last := messages[len(messages)-1]
	for budget := 1; budget <= 2000; budget += 17 {
		got := Trim(messages, Window{Max: budget})
		checkWindow(t, got)
		if len(got) == 0 || got[len(got)-1].Content != last.Content || got[len(got)-1].Role != last.Role {
			t.Fatalf("budget %d: newest message was trimmed away: %+v", budget, got)
		}
	}
}

func TestAnchorFindsTheNewestSingleRoundPrompt(t *testing.T) {
	turns := []Turn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
	}
	if messages, tokens := Anchor(turns); messages != 0 || tokens != 0 {
		t.Fatalf("anchor = %d messages, %d tokens; a turn with no usage cannot anchor", messages, tokens)
	}

	// A turn that ran tools stored the sum over its rounds, which is not the
	// size of any one prompt, so the newest plain turn is the one that anchors.
	turns = []Turn{
		{Role: "user", Text: "q1"},
		{Role: "tool_call", Text: mustJSON(toolCallRow{ID: "c1", Name: "shell", Arguments: "{}"})},
		{Role: "tool_result", Text: mustJSON(toolResultRow{ToolCallID: "c1", Name: "shell", Content: "ok"})},
		{Role: "assistant", Text: "a1", InputTokens: 999},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2", InputTokens: 70},
		{Role: "user", Text: "q3"},
	}
	messages, tokens := Anchor(turns)
	if tokens != 70 {
		t.Fatalf("anchor tokens = %d, want the newest plain prompt 70", tokens)
	}
	if want := len(MessagesFromTurns(turns[:5])); messages != want {
		t.Fatalf("anchor messages = %d, want %d (the messages the prompt carried)", messages, want)
	}
}

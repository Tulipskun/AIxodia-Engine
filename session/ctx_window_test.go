package session

import (
	"strings"
	"testing"

	"aixodia-engine/provider"
)

// sized builds a message whose estimated token count is easy to reason about:
// EstimateTokens is len/4+1 and MessageTokens adds 4 to each message.
func sized(role string, chars int) provider.Message {
	return provider.Message{Role: role, Content: strings.Repeat("x", chars)}
}

// callOf builds the assistant message that makes a tool call. Its estimate is 15:
// 4 for the empty body, 2 for the name, 1 for the arguments, 8 for the call.
func callOf(id string) provider.Message {
	return provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{
		{ID: id, Name: "shell", Arguments: "{}"},
	}}
}

// resultOf builds a tool result. Its estimate is 5: 1 for "ok" plus 4.
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

func TestTrimKeepsEverythingUnderBudget(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),
		sized("user", 40),
		sized("assistant", 40),
	}
	got := Trim(messages, 100000)
	if len(got) != len(messages) {
		t.Fatalf("got %d messages, want the whole conversation: %+v", len(got), got)
	}
	for i := range messages {
		if got[i].Role != messages[i].Role {
			t.Errorf("message %d = %q, want %q", i, got[i].Role, messages[i].Role)
		}
	}
}

// The window slides from the newest side: the oldest turn goes first, and the
// turn being asked stays.
func TestTrimDropsTheOldestTurn(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),     // 15
		sized("user", 400),      // 105
		sized("assistant", 400), // 105
		sized("user", 400),      // 105
		sized("assistant", 400), // 105
		sized("user", 400),      // 105
		sized("assistant", 400), // 105
	}
	// Two turns of 210 fit beside the 15-token system block; the third does not.
	got := Trim(messages, 15+420)
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

// A tool call and the results answering it are one turn. A budget that could
// hold the turn's tail but not the whole turn drops the tail too, instead of
// leaving a window that opens on an assistant or on an orphaned result.
func TestTrimNeverSplitsAToolTurn(t *testing.T) {
	messages := []provider.Message{
		sized("system", 40),     // 15
		sized("user", 400),      // 105
		callOf("c1"),            // 15
		resultOf("c1"),          // 5
		sized("user", 400),      // 105
		sized("assistant", 400), // 105
	}
	// 230 fits the newest turn (210) plus the tool result (5) and call (15) of
	// the older one, but the older turn as a whole is 125 and cannot join.
	got := Trim(messages, 15+230)
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
	got := Trim(messages, 15+10)
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
	for budget := 1; budget <= 4000; budget += 37 {
		got := Trim(messages, budget)
		checkWindow(t, got)
		if len(got) == 0 || got[len(got)-1].Content != last.Content || got[len(got)-1].Role != last.Role {
			t.Fatalf("budget %d: newest message was trimmed away: %+v", budget, got)
		}
	}
}

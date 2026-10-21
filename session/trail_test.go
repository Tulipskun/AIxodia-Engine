package session

import (
	"encoding/json"
	"testing"

	"aixodia-engine/provider"
)

func TestTrailRoundTripsThroughTurns(t *testing.T) {
	trail := []provider.Message{
		{Role: "assistant", Content: "checking", ToolCalls: []provider.ToolCall{
			{ID: "a", Name: "current_time", Arguments: "{}"},
			{ID: "b", Name: "weather", Arguments: `{"city":"Bangkok"}`},
		}},
		{Role: "tool", ToolCallID: "a", ToolName: "current_time", Content: "10:00"},
		{Role: "tool", ToolCallID: "b", ToolName: "weather", Content: "hot"},
	}

	var turns []Turn
	for _, input := range TrailTurns(trail) {
		turns = append(turns, Turn{Role: input.Role, Text: input.Text})
	}
	// The words before the calls are their own row: narration, call, call, result, result.
	if len(turns) != 5 {
		t.Fatalf("stored rows = %d, want 5: %+v", len(turns), turns)
	}

	rebuilt := MessagesFromTurns(turns)
	if len(rebuilt) != 5 {
		t.Fatalf("rebuilt = %d messages, want 5: %+v", len(rebuilt), rebuilt)
	}
	if rebuilt[0].Role != "assistant" || rebuilt[0].Content != "checking" {
		t.Errorf("narration = %+v, want a plain assistant turn", rebuilt[0])
	}
	if rebuilt[1].ToolCalls == nil || rebuilt[1].ToolCalls[0].ID != "a" {
		t.Errorf("first call lost its id: %+v", rebuilt[1])
	}
	if rebuilt[4].Role != "tool" || rebuilt[4].ToolCallID != "b" || rebuilt[4].Content != "hot" {
		t.Errorf("last result = %+v", rebuilt[4])
	}
}

func TestMessagesFromTurnsSkipsBrokenRows(t *testing.T) {
	rebuilt := MessagesFromTurns([]Turn{
		{Role: "tool_call", Text: "not json"},
		{Role: "tool_result", Text: `{"content":"orphan"}`},
		{Role: "user", Text: "still here"},
	})
	if len(rebuilt) != 1 || rebuilt[0].Content != "still here" {
		t.Errorf("rebuilt = %+v", rebuilt)
	}
}

// A D1 write whose HTTP answer was lost used to be retried and stored twice.
// Replaying those rows sent the provider two function_call items with one call
// id, and every later turn failed with "duplicate tool call ID".
func TestMessagesFromTurnsDropsARepeatedToolRound(t *testing.T) {
	call, _ := json.Marshal(toolCallRow{ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`})
	result, _ := json.Marshal(toolResultRow{ToolCallID: "c1", Name: "shell", Content: "a"})
	turns := []Turn{
		{Role: "assistant", Text: "checking"},
		{Role: "tool_call", Text: string(call)},
		{Role: "tool_result", Text: string(result)},
		{Role: "assistant", Text: "checking"},
		{Role: "tool_call", Text: string(call)},
		{Role: "tool_result", Text: string(result)},
		{Role: "user", Text: "next"},
	}
	got := MessagesFromTurns(turns)
	calls, results := 0, 0
	for _, message := range got {
		calls += len(message.ToolCalls)
		if message.Role == "tool" {
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("replayed %d calls and %d results, want one of each: %+v", calls, results, got)
	}
	if last := got[len(got)-1]; last.Role != "user" || last.Content != "next" {
		t.Errorf("last message = %+v, want the new question", last)
	}
}

// The reasoning a model wrote has to come back with the assistant turn, or the
// next request is refused for a missing reasoning_content.
func TestReasoningSurvivesTheRoundTrip(t *testing.T) {
	trail := []provider.Message{
		{Role: "assistant", Content: "เดี๋ยวผมลอง", ReasoningContent: "คิดไปสามวินาที", ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "tool", ToolCallID: "c1", ToolName: "bash", Content: "a"},
	}
	turns := TrailTurns(trail)
	turns = append(turns, TurnInput{Role: "assistant", Text: "เสร็จแล้ว", ReasoningContent: "คิดตอนจบ"})

	messages := MessagesFromTurns(toTurns(turns))
	if len(messages) != 4 {
		t.Fatalf("rebuilt %d messages, want 4: %+v", len(messages), messages)
	}
	if messages[1].ReasoningContent != "คิดไปสามวินาที" {
		t.Errorf("tool turn reasoning = %q, want the reasoning of the call that made it", messages[2].ReasoningContent)
	}
	if messages[3].ReasoningContent != "คิดตอนจบ" {
		t.Errorf("answer reasoning = %q, want the reasoning stored with it", messages[3].ReasoningContent)
	}
}

// toTurns is the read side of AppendTurns: the same rows the store hands back.
func toTurns(in []TurnInput) []Turn {
	out := make([]Turn, 0, len(in))
	for _, t := range in {
		out = append(out, Turn{Role: t.Role, Text: t.Text, ReasoningContent: t.ReasoningContent})
	}
	return out
}

func TestTrailTurnsKeepsNarrationOutOfToolRows(t *testing.T) {
	// A model that says something before it calls a tool: those words are its
	// own turn. Storing them inside the call row put the model's sentences
	// inside a tool block in the app.
	trail := []provider.Message{
		{Role: "assistant", Content: "กำลังเปิดดูไฟล์นะครับ", ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "read", Arguments: `{"filePath":"/a.png"}`},
		}},
		{Role: "tool", ToolCallID: "c1", ToolName: "read", Content: "bytes"},
	}

	turns := TrailTurns(trail)
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3 (narration, call, result): %+v", len(turns), turns)
	}
	if turns[0].Role != "assistant" || turns[0].Text != "กำลังเปิดดูไฟล์นะครับ" {
		t.Errorf("first turn = %+v, want the narration as an assistant turn", turns[0])
	}
	if turns[1].Role != "tool_call" {
		t.Errorf("second turn = %+v, want tool_call", turns[1])
	}
	var row toolCallRow
	if err := json.Unmarshal([]byte(turns[1].Text), &row); err != nil {
		t.Fatalf("tool_call row: %v", err)
	}
	if row.Content != "" {
		t.Errorf("tool row still carries text: %q", row.Content)
	}
}

func TestMessagesFromTurnsReadsNarrationBack(t *testing.T) {
	trail := []provider.Message{
		{Role: "assistant", Content: "เดี๋ยวผมลองอีกครั้ง", ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "tool", ToolCallID: "c1", ToolName: "bash", Content: "a b"},
	}
	turns := TrailTurns(trail)

	stored := make([]Turn, 0, len(turns))
	for i, in := range turns {
		stored = append(stored, Turn{Role: in.Role, Text: in.Text, Seq: i + 1})
	}
	got := MessagesFromTurns(stored)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(got), got)
	}
	if got[0].Role != "assistant" || got[0].Content != "เดี๋ยวผมลองอีกครั้ง" {
		t.Errorf("message 0 = %+v", got[0])
	}
	// The narration is a turn of its own; the call it led into is the next one.
	if len(got[0].ToolCalls) != 0 {
		t.Errorf("narration row should carry no tool calls: %+v", got[0])
	}
	if got[1].ToolCalls == nil || len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Name != "bash" {
		t.Errorf("the call itself should be the next message: %+v", got[1])
	}
	if got[2].Role != "tool" || got[2].Content != "a b" {
		t.Errorf("message 2 = %+v", got[2])
	}
}

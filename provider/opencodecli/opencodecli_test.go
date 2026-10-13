package opencodecli

import (
	"strings"
	"testing"

	"ai-engine/provider"
	"ai-engine/session"
)

// streamed is what `opencode run --format json` actually printed for a turn that
// ran one shell command and then answered. It is copied from the real client
// rather than written by hand, so the test fails if the client's shape moves.
const streamed = `{"type":"step_start","timestamp":1791882390344,"sessionID":"ses_x","part":{"id":"prt_1","type":"step-start"}}
{"type":"text","timestamp":1791882391054,"sessionID":"ses_x","part":{"id":"prt_2","type":"text","text":"กำลังรัน echo HELLO ให้ครับ","time":{"start":1791882391028,"end":1791882391054}}}
{"type":"tool_use","timestamp":1791882391381,"sessionID":"ses_x","part":{"id":"call_01a1","type":"tool","tool":"shell","state":{"status":"completed","input":{"command":"echo HELLO"},"output":"HELLO\n","title":"shell"}}}
{"type":"step_finish","timestamp":1791882391390,"sessionID":"ses_x","part":{"id":"prt_3","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"input":6975,"output":69,"reasoning":48,"cache":{"read":0,"write":0}}}}
{"type":"step_start","timestamp":1791882392793,"sessionID":"ses_x","part":{"id":"prt_4","type":"step-start"}}
{"type":"text","timestamp":1791882392861,"sessionID":"ses_x","part":{"id":"prt_5","type":"text","text":"ผลลัพธ์: HELLO","time":{"start":1791882392840,"end":1791882392861}}}
{"type":"step_finish","timestamp":1791882392980,"sessionID":"ses_x","part":{"id":"prt_6","type":"step-finish","reason":"stop","cost":0,"tokens":{"input":92,"output":20,"reasoning":32,"cache":{"read":7025,"write":0}}}}
`

func TestConsumeReadsTheClientsEvents(t *testing.T) {
	var deltas []string
	resp, err := (&Adapter{}).consume(strings.NewReader(streamed), func(text string) error {
		deltas = append(deltas, text)
		return nil
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	// The answer is every piece of text the client produced, in order.
	if resp.Content != "กำลังรัน echo HELLO ให้ครับผลลัพธ์: HELLO" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(deltas) != 2 {
		t.Errorf("the phone should have been sent two deltas, got %d", len(deltas))
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish reason = %q", resp.FinishReason)
	}
}

// The tokens are summed over the steps, because a turn that used tools asked the
// model more than once and each ask cost something.
func TestConsumeAddsUpTheTokens(t *testing.T) {
	resp, err := (&Adapter{}).consume(strings.NewReader(streamed), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 6975+92 {
		t.Errorf("prompt tokens = %d, want both steps", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 69+20 {
		t.Errorf("completion tokens = %d", resp.Usage.CompletionTokens)
	}
	if resp.Usage.ReasoningTokens != 48+32 {
		t.Errorf("reasoning tokens = %d", resp.Usage.ReasoningTokens)
	}
	if resp.Usage.CacheReadTokens != 7025 {
		t.Errorf("cache read = %d", resp.Usage.CacheReadTokens)
	}
}

// The trail is what the phone draws its tool blocks from, and it has to be the
// same shape this engine produces itself: the words before a call ride on the
// call row, and the result follows as a tool row.
func TestConsumeBuildsATrailThePhoneCanDraw(t *testing.T) {
	resp, err := (&Adapter{}).consume(strings.NewReader(streamed), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Trail) != 2 {
		t.Fatalf("trail = %d messages, want a call and its result", len(resp.Trail))
	}
	call := resp.Trail[0]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 {
		t.Fatalf("first trail message should be the call: %+v", call)
	}
	if call.ToolCalls[0].Name != "shell" {
		t.Errorf("tool name = %q", call.ToolCalls[0].Name)
	}
	if call.Content != "กำลังรัน echo HELLO ให้ครับ" {
		t.Errorf("the words before the call should ride on it, got %q", call.Content)
	}
	if !strings.Contains(call.ToolCalls[0].Arguments, "echo HELLO") {
		t.Errorf("arguments = %q", call.ToolCalls[0].Arguments)
	}
	result := resp.Trail[1]
	if result.Role != "tool" || result.ToolCallID != call.ToolCalls[0].ID {
		t.Errorf("the result must answer the call: %+v", result)
	}
	if result.Content != "HELLO\n" {
		t.Errorf("result content = %q", result.Content)
	}
}

// Stored turns are built from the trail, so the round boundaries have to survive
// the trip: narration, call, result, narration.
func TestTheTrailStoresAsTheSameRowsThisEngineWrites(t *testing.T) {
	resp, err := (&Adapter{}).consume(strings.NewReader(streamed), nil)
	if err != nil {
		t.Fatal(err)
	}
	turns := session.TrailTurns(resp.Trail)
	var roles []string
	for _, turn := range turns {
		roles = append(roles, turn.Role)
	}
	want := []string{"assistant", "tool_call", "tool_result"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Errorf("stored roles = %v, want %v", roles, want)
	}
}

// A turn with no tools is just text, and must not invent a trail.
func TestConsumeHandlesAPlainAnswer(t *testing.T) {
	plain := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"text","part":{"type":"text","text":"4"}}
{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"input":5579,"output":11,"reasoning":132,"cache":{"read":1393,"write":0}}}}
`
	resp, err := (&Adapter{}).consume(strings.NewReader(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "4" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.Trail) != 0 {
		t.Errorf("a plain answer has no trail, got %d", len(resp.Trail))
	}
	if resp.Usage.CacheReadTokens != 1393 {
		t.Errorf("cache read = %d", resp.Usage.CacheReadTokens)
	}
}

// The client prints progress and warnings on stdout as well; a line that is not
// an event must be skipped rather than ending the turn.
func TestConsumeSkipsLinesThatAreNotEvents(t *testing.T) {
	noisy := "building...\n" + streamed + "\nnot json either\n"
	resp, err := (&Adapter{}).consume(strings.NewReader(noisy), nil)
	if err != nil {
		t.Fatalf("noise should not fail the turn: %v", err)
	}
	if resp.Content == "" {
		t.Error("the events after the noise should still have been read")
	}
}

// The whole conversation goes to the client every turn, because the client has
// nowhere to keep it: its own store is in the container, and the container ends
// when the session does. So what this renders is what the model gets, and the
// question has to be distinguishable from the history around it.
func TestRenderConversationKeepsTheQuestionApartFromTheHistory(t *testing.T) {
	got := renderConversation([]provider.Message{
		{Role: "system", Content: "you are a helpful agent"},
		{Role: "user", Content: "ฉันชื่อสมชาย"},
		{Role: "assistant", Content: "ยินดีที่รู้จักครับ"},
		{Role: "user", Content: "  ฉันชื่ออะไร  "},
	})

	if !strings.Contains(got, "you are a helpful agent") {
		t.Error("the system prompt has to survive: it is the only copy the model sees")
	}
	if !strings.Contains(got, "<previous conversation>") || !strings.Contains(got, "</previous conversation>") {
		t.Errorf("the history should be inside a marked block:\n%s", got)
	}
	if !strings.Contains(got, "user: ฉันชื่อสมชาย") {
		t.Error("earlier turns are history and belong in the block")
	}
	if !strings.HasSuffix(got, "user: ฉันชื่ออะไร\n") {
		t.Errorf("the question belongs outside the block, at the end:\n%s", got)
	}
	if strings.Count(got, "ฉันชื่ออะไร") != 1 {
		t.Error("the question must not also appear in the history")
	}
}

// A tool step is one line each: enough to follow what was already done without
// turning the transcript into the tool's own output.
func TestRenderConversationWritesToolStepsAsOneLineEach(t *testing.T) {
	got := renderConversation([]provider.Message{
		{Role: "user", Content: "รัน pwd"},
		{Role: "assistant", Content: "กำลังรัน", ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "shell", Arguments: `{"command":"pwd"}`},
		}},
		{Role: "tool", Content: "/kaggle/working\n\nsecond line", ToolCallID: "c1", ToolName: "shell"},
		{Role: "assistant", Content: "เสร็จแล้ว"},
		{Role: "user", Content: "อีกที"},
	})

	if !strings.Contains(got, `[tool shell] {"command":"pwd"}`) {
		t.Errorf("the call should be one line:\n%s", got)
	}
	if !strings.Contains(got, "[result] /kaggle/working second line") {
		t.Errorf("the result should be flattened to one line:\n%s", got)
	}
	if !strings.Contains(got, "assistant: กำลังรัน") || !strings.Contains(got, "assistant: เสร็จแล้ว") {
		t.Errorf("the words before and after the call both belong in the history:\n%s", got)
	}
}

func TestRenderConversationWithoutAQuestionSendsNothing(t *testing.T) {
	if got := renderConversation([]provider.Message{{Role: "assistant", Content: "x"}}); got != "" {
		t.Errorf("with no question there is nothing to send, got %q", got)
	}
}

func TestOneLineFlattensAndCaps(t *testing.T) {
	if got := oneLine("a\n\nb   c", 100); got != "a b c" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 50)
	if got := oneLine(long, 10); got != strings.Repeat("x", 10)+"…" {
		t.Errorf("got %q", got)
	}
}

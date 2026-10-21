package session

import (
	"testing"
)

func TestBlockJSONRoundTrip(t *testing.T) {
	b := ToolBlock("c1", "shell", `{"command":"pwd"}`, "/tmp", false, 400)
	parsed, ok := ParseBlock(b.JSON())
	if !ok {
		t.Fatalf("ParseBlock(%s) = false", b.JSON())
	}
	if parsed != b {
		t.Errorf("round trip = %+v, want %+v", parsed, b)
	}
}

func TestParseBlockRejectsOldRows(t *testing.T) {
	for _, s := range []string{"", "{}", `{"id":"x"}`, "not json", `{"type":""}`} {
		if _, ok := ParseBlock(s); ok {
			t.Errorf("ParseBlock(%q) = true, want false", s)
		}
	}
}

func TestReasoningSharesRequestID(t *testing.T) {
	if got := ReasoningBlock(2, "คิด").ID; got != RequestID(2) {
		t.Errorf("reasoning id = %q, want %q", got, RequestID(2))
	}
	if got := RequestBlock(2, StateWaiting).ID; got != RequestID(2) {
		t.Errorf("request id = %q, want %q", got, RequestID(2))
	}
}

func TestToolBlockStates(t *testing.T) {
	if got := ToolBlock("c", "shell", "pwd", "", false, 0).State; got != StateRunning {
		t.Errorf("running state = %q", got)
	}
	if got := ToolBlock("c", "shell", "pwd", "ok", false, 9).State; got != StateDone {
		t.Errorf("done state = %q", got)
	}
	if got := ToolBlock("c", "shell", "pwd", "boom", true, 9).State; got != StateError {
		t.Errorf("error state = %q", got)
	}
}

func TestBlocksRideOnTurns(t *testing.T) {
	in := TurnInput{Role: "assistant", Text: "เสร็จแล้ว", Block: TextBlock("answer", "เสร็จแล้ว").JSON()}
	got := StoredTurn("s", 3, 9, in)
	if got.Block != in.Block {
		t.Errorf("StoredTurn block = %q, want %q", got.Block, in.Block)
	}
	if _, ok := ParseBlock(got.Block); !ok {
		t.Errorf("stored block %q does not parse", got.Block)
	}
	// Old rows carry nothing and must stay readable as plain text.
	old := StoredTurn("s", 2, 9, TurnInput{Role: "assistant", Text: "เก่า"})
	if _, ok := ParseBlock(old.Block); ok {
		t.Errorf("empty block parsed, want fallback")
	}
}

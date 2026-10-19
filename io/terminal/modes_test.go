package terminal

import (
	"strings"
	"testing"
)

func TestModeTrackerReplaysWhatTheProgramLeftOn(t *testing.T) {
	tr := newModeTracker()
	tr.scan([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h"))

	got := string(tr.replay())
	want := "\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h"
	if got != want {
		t.Fatalf("replay = %q, want %q", got, want)
	}
}

func TestModeTrackerJoinsSequencesSplitAcrossReads(t *testing.T) {
	tr := newModeTracker()
	tr.scan([]byte("\x1b[?10"))
	tr.scan([]byte("03h\x1b[?1006"))
	tr.scan([]byte("h"))

	got := string(tr.replay())
	if !strings.Contains(got, "\x1b[?1003h") || !strings.Contains(got, "\x1b[?1006h") {
		t.Fatalf("split sequence not tracked: %q", got)
	}
}

func TestModeTrackerDropsModesTurnedBackOff(t *testing.T) {
	tr := newModeTracker()
	tr.scan([]byte("\x1b[?1003h\x1b[?1049h"))
	tr.scan([]byte("\x1b[?1003l\x1b[?1049l"))

	if got := string(tr.replay()); got != "" {
		t.Fatalf("replay of disabled modes = %q, want empty", got)
	}
}

func TestModeTrackerIgnoresSequencesInsideStrings(t *testing.T) {
	tr := newModeTracker()
	// An OSC title that happens to contain what looks like a mode set, then a
	// DCS with another one. Neither is terminal state.
	tr.scan([]byte("\x1b]0;evil\x1b[?1003h\x07\x1bPq\x1b[?1049h\x1b\\"))

	if got := string(tr.replay()); got != "" {
		t.Fatalf("string payload leaked into modes: %q", got)
	}
}

func TestModeTrackerHandlesQueriesAndHiddenCursor(t *testing.T) {
	tr := newModeTracker()
	// A capability query with an intermediate byte is not a mode change.
	tr.scan([]byte("\x1b[?1016$p\x1b[?2027$p\x1b[?25l\x1b[?7l"))

	got := string(tr.replay())
	if strings.Contains(got, "1016") || strings.Contains(got, "2027") {
		t.Fatalf("query replayed as a mode: %q", got)
	}
	if !strings.Contains(got, "\x1b[?25l") || !strings.Contains(got, "\x1b[?7l") {
		t.Fatalf("hidden cursor or no-wrap not replayed: %q", got)
	}
}

func TestModeTrackerDoesNotReplayDefaults(t *testing.T) {
	tr := newModeTracker()
	tr.scan([]byte("\x1b[?25h\x1b[?7h\x1b[?1l"))

	if got := string(tr.replay()); got != "" {
		t.Fatalf("default modes replayed: %q", got)
	}
}

// Only a program on the alternate screen needs the repaint nudge: a shell
// prompt is fully described by its ring, and signalling it would draw the line
// being edited a second time.
func TestModeTrackerKnowsTheAlternateScreen(t *testing.T) {
	tr := newModeTracker()
	if tr.altScreen() {
		t.Fatal("a fresh tracker is on the normal screen")
	}
	tr.scan([]byte("\x1b[?1049h"))
	if !tr.altScreen() {
		t.Fatal("1049h did not enter the alternate screen")
	}
	tr.scan([]byte("\x1b[?1049l"))
	if tr.altScreen() {
		t.Fatal("1049l did not leave the alternate screen")
	}
	tr.scan([]byte("\x1b[?47h"))
	if !tr.altScreen() {
		t.Fatal("47h did not enter the alternate screen")
	}
}

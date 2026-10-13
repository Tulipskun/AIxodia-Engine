package opencode

import (
	"strconv"
	"strings"
	"testing"
)

// The session id is the one part of the request the free tier reads, and what
// it reads is the counter in the low twelve bits of the head: 63 and below is
// accepted, 64 and above is refused. Random bytes scatter that counter, which is
// how the engine spent a month being refused for ids that looked correct.
func TestSessionIDCounterIsInsideTheAcceptedRange(t *testing.T) {
	id := newID("ses_")
	head := strings.TrimPrefix(id, "ses_")
	if len(head) != 26 {
		t.Fatalf("id %q: want 26 characters after the prefix, got %d", id, len(head))
	}
	encoded, err := strconv.ParseUint(head[:12], 16, 64)
	if err != nil {
		t.Fatalf("id %q: head is not hex: %v", id, err)
	}
	// The head is stored inverted, so the counter is read back out of the low
	// twelve bits the same way.
	counter := 0xfff - int(encoded&0xfff)
	if counter < 1 || counter > 63 {
		t.Errorf("id %q carries counter %d; the gate refuses anything above 63", id, counter)
	}
	if tail := head[12:]; !isBase62(tail) {
		t.Errorf("id %q: tail %q has a character the gate refuses", id, tail)
	}
}

func isBase62(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// Two ids minted in the same millisecond must not collide: the counter exists
// for that, and the tail alone is not enough to rely on.
func TestIDsFromTheSameMillisecondDiffer(t *testing.T) {
	seen := make(map[string]bool, 512)
	for range 512 {
		id := newID("ses_")
		if seen[id] {
			t.Fatalf("id %q was minted twice", id)
		}
		seen[id] = true
	}
}

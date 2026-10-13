package opencode

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The session id is the one part of the request the free tier actually reads,
// and it reads it as a time. An id that does not carry a current one is refused
// whatever else about the request is right, which is why this is asserted here
// rather than left to the shape of the id to imply.
func TestSessionIDCarriesTheCurrentTime(t *testing.T) {
	id := newID("ses_")
	head := strings.TrimPrefix(id, "ses_")
	if len(head) != 26 {
		t.Fatalf("id %q: want 26 characters after the prefix, got %d", id, len(head))
	}
	encoded, err := strconv.ParseUint(head[:12], 16, 64)
	if err != nil {
		t.Fatalf("id %q: head is not hex: %v", id, err)
	}
	// The client inverts the timestamp so ids sort newest-first, and only six
	// bytes of it survive, so reading it back means subtracting from what is
	// left of those six bytes and dropping the twelve the counter uses. Six
	// bytes cannot hold the full millisecond count, so the time comes back
	// wrapped into a period of about two years and has to be unwrapped against
	// the clock.
	const kept = uint64(1) << 48
	const period = kept >> 12
	back := (kept - encoded%kept) % kept
	stamped := int64(back >> 12)
	age := time.Duration((uint64(time.Now().UnixMilli())-uint64(stamped))%period) * time.Millisecond
	if age > time.Hour {
		t.Errorf("id %q encodes a time %v away; the gate would read it as stale", id, age)
	}
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

package main

import (
	"context"
	"encoding/json"
	"testing"

	"aixodia-engine/io/gateway"
	"aixodia-engine/session"
)

// handoverStore answers only the one call the note makes: the questions the
// previous session left unanswered.
type handoverStore struct {
	gateway.SessionStore
	turns []session.UnfinishedTurn
}

func (s handoverStore) Unfinished(int) ([]session.UnfinishedTurn, error) { return s.turns, nil }

// The note is how a new session learns whether the one before it was cut off
// mid-turn. It is a key in D1's system_state beside the daemon keys, and it
// lists the unfinished turns the session store reports.
func TestPublishHandoverWritesTheNote(t *testing.T) {
	sessionStore = handoverStore{turns: []session.UnfinishedTurn{
		{SessionID: "s1", Seq: 3, Text: "ยังไม่ได้ตอบ"},
	}}
	t.Cleanup(func() { sessionStore = nil })
	was := startedAt
	startedAt = 1700000000
	t.Cleanup(func() { startedAt = was })

	var stored string
	wasPut := putHandoverState
	putHandoverState = func(key, value string) error {
		if key != handoverStateKey {
			t.Fatalf("wrote key %q, want %q", key, handoverStateKey)
		}
		stored = value
		return nil
	}
	t.Cleanup(func() { putHandoverState = wasPut })

	publishHandover(context.Background(), "https://handover.test")

	if stored == "" {
		t.Fatal("the handover note was not written")
	}
	var note Handover
	if err := json.Unmarshal([]byte(stored), &note); err != nil {
		t.Fatalf("note is not JSON: %v (%s)", err, stored)
	}
	if note.Tunnel != "https://handover.test" || note.KernelStarted != 1700000000 {
		t.Fatalf("note = %+v, want the tunnel and the kernel start time", note)
	}
	if len(note.Unfinished) != 1 || note.Unfinished[0].Text != "ยังไม่ได้ตอบ" {
		t.Fatalf("unfinished = %+v, want the question the last session left", note.Unfinished)
	}
}

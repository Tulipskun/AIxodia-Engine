package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"aixodia-engine/localdb"
	"aixodia-engine/session"
)

// The note is how a new session learns whether the one before it was cut off
// mid-turn. It used to be a D1 row; it is now a key in the engine's own file,
// which is also where the unfinished turns it lists come from.
func TestPublishHandoverWritesTheLocalNote(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	storeDB = handle
	sessionStore = session.NewSQLite(handle)
	t.Cleanup(func() {
		storeDB = nil
		sessionStore = nil
	})
	was := startedAt
	startedAt = 1700000000
	t.Cleanup(func() { startedAt = was })

	created, err := sessionStore.Create(session.NewSession{Title: "ค้างไว้"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionStore.AppendTurns(created.ID, []session.TurnInput{{Role: "user", Text: "ยังไม่ได้ตอบ"}}); err != nil {
		t.Fatal(err)
	}

	publishHandover(context.Background(), "https://handover.test")

	stored, found, err := localdb.GetState(handle, handoverStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the handover note was not written to the local store")
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

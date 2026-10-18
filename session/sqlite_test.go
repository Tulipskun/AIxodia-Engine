package session

import (
	"path/filepath"
	"testing"
	"time"

	"ai-engine/localdb"
)

// List must return. It used to open a second query without draining or closing
// the first, and the pool is capped at one connection, so the second query
// waited forever — GET /v1/sessions hung instead of answering.
func TestSQLiteListReturns(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	st := NewSQLite(handle)
	if _, err := st.Create(NewSession{Title: "t"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan []Session, 1)
	go func() {
		got, err := st.List()
		if err != nil {
			t.Errorf("List: %v", err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].Title != "t" {
			t.Fatalf("List = %+v, want one session titled t", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("List blocked: a second query waits for the only connection")
	}
}

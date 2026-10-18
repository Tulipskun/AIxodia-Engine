package session

import (
	"path/filepath"
	"testing"
	"time"
)

// List must return. It used to open a second query without draining or closing
// the first, and the pool is capped at one connection, so the second query
// waited forever — GET /v1/sessions hung instead of answering.
func TestSQLiteListReturns(t *testing.T) {
	st, err := NewSQLite(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
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

// Import is how history that predates the local store is moved over, so it has
// to keep the ids and sequence numbers the phone already knows, and running it
// again must change nothing.
func TestSQLiteImportKeepsIDsAndIsRepeatable(t *testing.T) {
	st, err := NewSQLite(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := Session{ID: "abc123", Title: "old chat", Model: "Opencode/m", CreatedAt: 100, UpdatedAt: 200}
	turns := []Turn{
		{SessionID: "abc123", Seq: 1, Role: "user", Text: "hi", CreatedAt: 101},
		{SessionID: "abc123", Seq: 2, Role: "assistant", Text: "hello", CreatedAt: 102, InputTokens: 5},
	}

	added, n, err := st.Import(sess, turns)
	if err != nil {
		t.Fatal(err)
	}
	if !added || n != 2 {
		t.Fatalf("Import = %v, %d; want true, 2", added, n)
	}

	got, err := st.Get("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "old chat" || got.CreatedAt != 100 || got.UpdatedAt != 200 {
		t.Fatalf("imported session = %+v, want the stored title and timestamps", got)
	}
	stored, err := st.Turns("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Seq != 1 || stored[1].Seq != 2 || stored[1].Text != "hello" || stored[1].InputTokens != 5 {
		t.Fatalf("imported turns = %+v, want seq 1 and 2 in order", stored)
	}

	added, n, err = st.Import(sess, turns)
	if err != nil {
		t.Fatal(err)
	}
	if added || n != 0 {
		t.Fatalf("second Import = %v, %d; want false, 0", added, n)
	}
}

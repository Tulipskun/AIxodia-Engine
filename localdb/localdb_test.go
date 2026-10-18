package localdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Everything the engine owns lives in one file, so every table has to exist the
// moment it is opened: a missing one would surface later as a failing request
// instead of a failed boot.
func TestOpenCreatesEveryTableTheEngineUses(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"chat_sessions", "chat_turns", "llm_providers", "state"} {
		var name string
		if err := store.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

// Kaggle's writable directory is /kaggle/working, but the store can be pointed
// at a directory that does not exist yet.
func TestOpenCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "aixodia.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Ping(); err != nil {
		t.Fatal(err)
	}
}

// AIXODIA_DB_PATH is how a machine says where the store lives.
func TestPathUsesTheEnvironmentFirst(t *testing.T) {
	want := filepath.Join(t.TempDir(), "chosen.db")
	t.Setenv("AIXODIA_DB_PATH", want)
	if got := Path(); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

// Without the environment there is still one answer, and it names the store.
func TestPathAlwaysNamesTheStore(t *testing.T) {
	t.Setenv("AIXODIA_DB_PATH", "")
	if got := Path(); !strings.HasSuffix(got, "aixodia.db") {
		t.Fatalf("Path() = %q, want a path ending in aixodia.db", got)
	}
}

func TestOpenWithoutAPathUsesPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.db")
	t.Setenv("AIXODIA_DB_PATH", path)
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

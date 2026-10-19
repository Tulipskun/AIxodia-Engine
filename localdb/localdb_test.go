package localdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Everything the engine owns lives in one file, so every table has to exist the
// moment it is opened: a missing one would surface later as a failing request
// instead of a failed boot. Providers are not here: they live in D1.
func TestOpenCreatesEveryTableTheEngineUses(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"chat_sessions", "chat_turns", "state"} {
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

// There is still one answer, and it names the store.
func TestPathAlwaysNamesTheStore(t *testing.T) {
	if got := Path(); !strings.HasSuffix(got, "aixodia.db") {
		t.Fatalf("Path() = %q, want a path ending in aixodia.db", got)
	}
}

func TestOpenWithoutAPathUsesPath(t *testing.T) {
	dir := t.TempDir()
	was, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(was) })
	path := Path()
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
}

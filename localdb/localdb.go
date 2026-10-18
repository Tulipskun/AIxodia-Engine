// Package localdb owns the engine's store: one SQLite file holding everything
// the engine used to keep in D1 — chat history, providers (with their API keys),
// settings and the daemon's own notes. D1 is left with discovery only.
//
// One file, one handle: the pool is capped at a single connection so two
// writers cannot fight over SQLite's file lock, and that handle is handed to
// every package that stores something.
package localdb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Path is where the store lives. AIXODIA_DB_PATH wins; Kaggle's writable
// directory is /kaggle/working; a development checkout keeps it beside itself.
func Path() string {
	if path := strings.TrimSpace(os.Getenv("AIXODIA_DB_PATH")); path != "" {
		return path
	}
	if st, err := os.Stat("/kaggle/working"); err == nil && st.IsDir() {
		return "/kaggle/working/aixodia.db"
	}
	return "./aixodia.db"
}

// Open opens (and creates) the store and brings its schema up to date. An empty
// path means Path().
func Open(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		path = Path()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("localdb: create %s: %w", dir, err)
		}
	}

	// busy_timeout is what keeps a write from failing outright when another
	// process holds the file: a forgotten engine or a sqlite3 shell finishes its
	// statement instead, and this one waits.
	handle, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("localdb: open %s: %w", path, err)
	}
	handle.SetMaxOpenConns(1)
	if err := migrate(handle); err != nil {
		handle.Close()
		return nil, fmt.Errorf("localdb: migrate %s: %w", path, err)
	}
	return handle, nil
}

// The chat tables were the local store's own schema before it held anything
// else; they are listed here so one file answers for all of it.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS chat_sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT (unixepoch()),
		updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
		sub_provider TEXT DEFAULT '',
		sub_model TEXT DEFAULT '',
		sub_enabled INTEGER DEFAULT 0,
		reasoning INTEGER NOT NULL DEFAULT 0,
		temperature REAL,
		top_p REAL,
		max_tokens INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS chat_turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		seq INTEGER NOT NULL,
		role TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT '',
		job_id TEXT NOT NULL DEFAULT '',
		text TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT (unixepoch()),
		model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		input_includes_cache INTEGER NOT NULL DEFAULT 0,
		generation_ms INTEGER NOT NULL DEFAULT 0,
		reasoning_content TEXT NOT NULL DEFAULT '',
		block TEXT NOT NULL DEFAULT '',
		UNIQUE(session_id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions(updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_turns_session_seq ON chat_turns(session_id, seq)`,
	// idx is D1's `index` column, which the engine reads as a provider's ID.
	// The name is the one SQLite accepts as a column without quoting.
	`CREATE TABLE IF NOT EXISTS llm_providers (
		idx INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL UNIQUE,
		endpoint TEXT NOT NULL DEFAULT '',
		keys TEXT NOT NULL DEFAULT '[]',
		adapter TEXT NOT NULL DEFAULT ''
	)`,
	// One key/value table for engine settings and the daemon's own notes: they
	// are written once and read whole, and D1's reasons for a column each
	// (queryability, a row per setting) do not apply to a file on this machine.
	`CREATE TABLE IF NOT EXISTS state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL DEFAULT (unixepoch())
	)`,
}

// Columns added after the store's first release. ALTER is the only migration
// SQLite offers for an existing table, and a duplicate column is what every
// boot after the first answers with, so the error is expected and ignored.
var addedColumns = []string{
	`ALTER TABLE chat_turns ADD COLUMN generation_ms INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_turns ADD COLUMN reasoning_content TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_turns ADD COLUMN block TEXT NOT NULL DEFAULT ''`,
}

func migrate(handle *sql.DB) error {
	for _, statement := range schema {
		if _, err := handle.Exec(statement); err != nil {
			return err
		}
	}
	for _, column := range addedColumns {
		_, _ = handle.Exec(column)
	}
	return nil
}

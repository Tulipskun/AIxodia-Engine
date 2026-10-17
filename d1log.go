package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"ai-engine/db"
)

// d1LogEnabled flips on once the daemon has verified its D1 credentials, so
// logging never fires network calls during tests or before configuration.
var d1LogEnabled bool

const maxEngineLogRows = 5000
const maxArchivedLogTables = 3

type logEntry struct {
	level string
	msg   string
}

var (
	logCh   = make(chan logEntry, 2048)
	logOnce sync.Once
)

// startD1Logging spins up the single writer goroutine and tees the standard
// logger into it. Call once, from main, after db.Verify succeeds.
func startD1Logging() {
	logOnce.Do(func() {
		d1LogEnabled = true
		go logWriter()
		log.SetOutput(&teeWriter{w: os.Stdout})
	})
}

func logWriter() {
	for e := range logCh {
		if !d1LogEnabled {
			continue
		}
		// Rotate before inserting if the current table is full.
		if n, err := countEngineLogs("engine_logs"); err == nil && n >= maxEngineLogRows {
			rotateEngineLogs()
		}
		_, _ = db.Query(
			"INSERT INTO engine_logs (level, message, at) VALUES (?, ?, unixepoch())",
			e.level, e.msg,
		)
	}
}

func countEngineLogs(table string) (int, error) {
	if !tableExists(table) {
		return 0, nil
	}
	rows, err := db.Query("SELECT COUNT(*) AS c FROM " + table)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	v := rows[0]["c"]
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	}
	return 0, nil
}

func tableExists(name string) bool {
	rows, err := db.Query("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", name)
	if err != nil {
		return false
	}
	return len(rows) > 0
}

func createEngineLogsTable() {
	_, _ = db.Query(`CREATE TABLE IF NOT EXISTS engine_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level TEXT NOT NULL DEFAULT 'info',
		message TEXT NOT NULL DEFAULT '',
		at INTEGER NOT NULL DEFAULT (unixepoch())
	)`)
	_, _ = db.Query("CREATE INDEX IF NOT EXISTS idx_engine_logs_at ON engine_logs(at)")
}

func rotateEngineLogs() {
	// Drop the oldest archive.
	if tableExists("engine_logs_3") {
		_, _ = db.Query("DROP TABLE engine_logs_3")
	}
	// Shift archives down.
	if tableExists("engine_logs_2") {
		_, _ = db.Query("ALTER TABLE engine_logs_2 RENAME TO engine_logs_3")
	}
	if tableExists("engine_logs_1") {
		_, _ = db.Query("ALTER TABLE engine_logs_1 RENAME TO engine_logs_2")
	}
	if tableExists("engine_logs") {
		_, _ = db.Query("ALTER TABLE engine_logs RENAME TO engine_logs_1")
	}
	createEngineLogsTable()
}

// D1Log queues a structured log entry. Safe to call from any goroutine; it never
// blocks the caller (drops when the queue is full).
func D1Log(level, format string, args ...any) {
	if !d1LogEnabled {
		return
	}
	msg := fmt.Sprintf(format, args...)
	select {
	case logCh <- logEntry{level: level, msg: msg}:
	default:
	}
}

func LogInfo(format string, args ...any)  { D1Log("info", format, args...) }
func LogWarn(format string, args ...any)  { D1Log("warn", format, args...) }
func LogError(format string, args ...any) { D1Log("error", format, args...) }

type teeWriter struct{ w io.Writer }

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	msg := strings.TrimSpace(string(p))
	if msg != "" && d1LogEnabled {
		select {
		case logCh <- logEntry{level: "info", msg: msg}:
		default:
		}
	}
	return n, err
}

// Ensure the writer is used.
var _ io.Writer = (*teeWriter)(nil)

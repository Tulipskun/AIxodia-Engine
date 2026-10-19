package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Logs live on the engine machine (local SQLite), not in D1. D1 is only used
// for daemon discovery (the tunnel URL), which the phone must read remotely.

var logDB *sql.DB
var d1LogEnabled bool // kept for compat; always true once started

const maxEngineLogRows = 5000

type logEntry struct {
	level string
	msg   string
}

var (
	logCh   = make(chan logEntry, 2048)
	logOnce sync.Once
)

func logDBPath() string {
	if st, err := os.Stat("/kaggle/working"); err == nil && st.IsDir() {
		return "/kaggle/working/aixodia-logs.db"
	}
	return "./aixodia-logs.db"
}

func startD1Logging() {
	logOnce.Do(func() {
		path := logDBPath()
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0755)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			log.Printf("logs: cannot open db: %v", err)
			return
		}
		db.SetMaxOpenConns(1)
		logDB = db
		createEngineLogsTableLocal()
		d1LogEnabled = true
		go logWriter()
		log.SetOutput(&teeWriter{w: os.Stdout})
	})
}

func logWriter() {
	for e := range logCh {
		if logDB == nil {
			continue
		}
		if n, err := countEngineLogsLocal("engine_logs"); err == nil && n >= maxEngineLogRows {
			rotateEngineLogsLocal()
		}
		_, _ = logDB.Exec(
			"INSERT INTO engine_logs (level, message, at) VALUES (?, ?, unixepoch())",
			e.level, e.msg,
		)
	}
}

func countEngineLogsLocal(table string) (int, error) {
	rows, err := logDB.Query("SELECT COUNT(*) AS c FROM " + table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, nil
	}
	var c int
	if err := rows.Scan(&c); err != nil {
		return 0, err
	}
	return c, nil
}

func tableExistsLocal(name string) bool {
	rows, err := logDB.Query("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", name)
	if err != nil {
		return false
	}
	defer rows.Close()
	return rows.Next()
}

func createEngineLogsTableLocal() {
	if logDB == nil {
		return
	}
	_, _ = logDB.Exec(`CREATE TABLE IF NOT EXISTS engine_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level TEXT NOT NULL DEFAULT 'info',
		message TEXT NOT NULL DEFAULT '',
		at INTEGER NOT NULL DEFAULT (unixepoch())
	)`)
	_, _ = logDB.Exec("CREATE INDEX IF NOT EXISTS idx_engine_logs_at ON engine_logs(at)")
}

func rotateEngineLogsLocal() {
	if tableExistsLocal("engine_logs_3") {
		_, _ = logDB.Exec("DROP TABLE engine_logs_3")
	}
	if tableExistsLocal("engine_logs_2") {
		_, _ = logDB.Exec("ALTER TABLE engine_logs_2 RENAME TO engine_logs_3")
	}
	if tableExistsLocal("engine_logs_1") {
		_, _ = logDB.Exec("ALTER TABLE engine_logs_1 RENAME TO engine_logs_2")
	}
	if tableExistsLocal("engine_logs") {
		_, _ = logDB.Exec("ALTER TABLE engine_logs RENAME TO engine_logs_1")
	}
	createEngineLogsTableLocal()
}

func D1Log(level, format string, args ...any) {
	if logDB == nil {
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

func recentLogs(limit int, level string) []map[string]any {
	if logDB == nil {
		return nil
	}
	tables := []string{"engine_logs"}
	for i := 1; i <= 3; i++ {
		t := "engine_logs_" + fmt.Sprintf("%d", i)
		if tableExistsLocal(t) {
			tables = append(tables, t)
		}
	}
	q := ""
	for i, t := range tables {
		if i > 0 {
			q += " UNION ALL "
		}
		q += "SELECT id, level, message, at, '" + t + "' AS tbl FROM " + t
	}
	if level != "" {
		q = "SELECT * FROM (" + q + ") WHERE level = ?"
	}
	q += " ORDER BY at DESC LIMIT " + fmt.Sprintf("%d", limit)
	var rows *sql.Rows
	var err error
	if level != "" {
		rows, err = logDB.Query(q, level)
	} else {
		rows, err = logDB.Query(q)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out
}

type teeWriter struct{ w io.Writer }

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	msg := strings.TrimSpace(string(p))
	if msg != "" && logDB != nil {
		select {
		case logCh <- logEntry{level: "info", msg: msg}:
		default:
		}
	}
	return n, err
}

package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ai-engine/provider"

	_ "modernc.org/sqlite"

	"ai-engine/db"
)

type SQLite struct {
	db *sql.DB
}

func NewSQLite(path string) (*SQLite, error) {
	if path == "" {
		if st, err := os.Stat("/kaggle/working"); err == nil && st.IsDir() {
			path = "/kaggle/working/aixodia.db"
		} else {
			path = "./aixodia.db"
		}
	}
	// Ensure parent dir exists.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate() error {
	stmts := []string{
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
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return err
		}
	}
	// Ensure turns columns exist (idempotent).
	for _, col := range []string{
		`ALTER TABLE chat_turns ADD COLUMN generation_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_turns ADD COLUMN reasoning_content TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_turns ADD COLUMN block TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = s.db.Exec(col) // ignore duplicate column errors
	}
	return nil
}

// MigrateFromD1 copies chat history from D1 into this SQLite store if the
// store is empty. Idempotent: does nothing when rows already exist.
func (s *SQLite) MigrateFromD1() error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM chat_sessions").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// Read from D1.
	rows, err := db.Query("SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions")
	if err != nil {
		return err
	}
	for _, r := range rows {
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO chat_sessions (id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			str(r["id"]), str(r["title"]), str(r["provider"]), str(r["model"]), str(r["sub_provider"]), str(r["sub_model"]),
			intVal(r["sub_enabled"]), intVal(r["reasoning"]), floatPtr(r["temperature"]), floatPtr(r["top_p"]), intVal(r["max_tokens"]),
			int64Val(r["created_at"]), int64Val(r["updated_at"]))
	}
	trows, err := db.Query("SELECT session_id,seq,role,agent,job_id,text,created_at,model,input_tokens,output_tokens,duration_ms,cache_read_tokens,cache_write_tokens,reasoning_tokens,input_includes_cache,generation_ms,reasoning_content,block FROM chat_turns")
	if err != nil {
		return err
	}
	for _, r := range trows {
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO chat_turns (session_id,seq,role,agent,job_id,text,created_at,model,input_tokens,output_tokens,duration_ms,cache_read_tokens,cache_write_tokens,reasoning_tokens,input_includes_cache,generation_ms,reasoning_content,block) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			str(r["session_id"]), intVal(r["seq"]), str(r["role"]), str(r["agent"]), str(r["job_id"]), str(r["text"]), int64Val(r["created_at"]), str(r["model"]),
			intVal(r["input_tokens"]), intVal(r["output_tokens"]), intVal(r["duration_ms"]), intVal(r["cache_read_tokens"]), intVal(r["cache_write_tokens"]), intVal(r["reasoning_tokens"]),
			intVal(r["input_includes_cache"]), intVal(r["generation_ms"]), str(r["reasoning_content"]), str(r["block"]))
	}
	return nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
func intVal(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	}
	return 0
}
func int64Val(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		var n int64
		fmt.Sscanf(t, "%d", &n)
		return n
	}
	return 0
}
func floatPtr(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case nil:
		return nil
	}
	return nil
}


func (s *SQLite) Create(params NewSession) (*Session, error) {
	now := time.Now().Unix()
	session := Session{
		ID:          newID(),
		Title:       params.Title,
		Provider:    params.Provider,
		Model:       params.Model,
		SubProvider: params.SubProvider,
		SubModel:    params.SubModel,
		SubEnabled:  params.SubEnabled,
		Reasoning:   params.Reasoning,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   params.MaxTokens,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err := s.db.Exec(`INSERT INTO chat_sessions (id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		session.ID, session.Title, session.Provider, session.Model, session.SubProvider, session.SubModel, boolInt(session.SubEnabled), boolInt(session.Reasoning),
		session.Temperature, session.TopP, session.MaxTokens, session.CreatedAt, session.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *SQLite) Get(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions WHERE id=?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *SQLite) List() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Scan() == nil {
		// placeholder; handled below
	}
	// Re-scan properly
	rows2, err := s.db.Query(`SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		sess, err := scanSessionRows(rows2)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows2.Err()
}

func scanSession(row *sql.Row) (*Session, error) {
	var sess Session
	var subEnabled, reasoning int
	var temp, topP sql.NullFloat64
	var maxTokens sql.NullInt64
	err := row.Scan(&sess.ID, &sess.Title, &sess.Provider, &sess.Model, &sess.SubProvider, &sess.SubModel, &subEnabled, &reasoning, &temp, &topP, &maxTokens, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %q not found", sess.ID)
		}
		return nil, err
	}
	sess.SubEnabled = subEnabled != 0
	sess.Reasoning = reasoning != 0
	if temp.Valid {
		v := temp.Float64
		sess.Temperature = &v
	}
	if topP.Valid {
		v := topP.Float64
		sess.TopP = &v
	}
	if maxTokens.Valid {
		sess.MaxTokens = int(maxTokens.Int64)
	}
	return &sess, nil
}

func scanSessionRows(rows *sql.Rows) (*Session, error) {
	var sess Session
	var subEnabled, reasoning int
	var temp, topP sql.NullFloat64
	var maxTokens sql.NullInt64
	err := rows.Scan(&sess.ID, &sess.Title, &sess.Provider, &sess.Model, &sess.SubProvider, &sess.SubModel, &subEnabled, &reasoning, &temp, &topP, &maxTokens, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, err
	}
	sess.SubEnabled = subEnabled != 0
	sess.Reasoning = reasoning != 0
	if temp.Valid {
		v := temp.Float64
		sess.Temperature = &v
	}
	if topP.Valid {
		v := topP.Float64
		sess.TopP = &v
	}
	if maxTokens.Valid {
		sess.MaxTokens = int(maxTokens.Int64)
	}
	return &sess, nil
}

func (s *SQLite) Delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM chat_turns WHERE session_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM chat_sessions WHERE id=?`, id)
	return err
}

func (s *SQLite) SaveConfig(id string, session Session) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE chat_sessions SET provider=?, model=?, title=CASE WHEN ? != '' THEN ? ELSE title END, updated_at=? WHERE id=?`,
		session.Provider, session.Model, session.Title, session.Title, now, id)
	return err
}

func (s *SQLite) Turns(id string) ([]Turn, error) {
	rows, err := s.db.Query(`SELECT session_id,seq,role,agent,job_id,text,created_at,model,input_tokens,output_tokens,duration_ms,cache_read_tokens,cache_write_tokens,reasoning_tokens,input_includes_cache,generation_ms,reasoning_content,block FROM chat_turns WHERE session_id=? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		var inCache int
		if err := rows.Scan(&t.SessionID, &t.Seq, &t.Role, &t.Agent, &t.JobID, &t.Text, &t.CreatedAt, &t.Model, &t.InputTokens, &t.OutputTokens, &t.DurationMS, &t.CacheReadTokens, &t.CacheWriteTokens, &t.ReasoningTokens, &inCache, &t.GenerationMS, &t.ReasoningContent, &t.Block); err != nil {
			return nil, err
		}
		t.InputIncludesCache = inCache != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) History(id string) ([]provider.Message, error) {
	turns, err := s.Turns(id)
	if err != nil {
		return nil, err
	}
	return MessagesFromTurns(turns), nil
}

func (s *SQLite) AppendTurns(id string, turns []TurnInput) ([]Turn, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// next seq
	var maxSeq int
	row := tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM chat_turns WHERE session_id=?`, id)
	if err := row.Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := maxSeq + 1
	now := time.Now().Unix()
	var saved []Turn
	for i, turn := range turns {
		sq := seq + i
		_, err := tx.Exec(`INSERT INTO chat_turns (session_id,seq,role,text,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,reasoning_content,input_includes_cache,duration_ms,generation_ms,block,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, sq, turn.Role, turn.Text, turn.Model, turn.InputTokens, turn.OutputTokens, turn.CacheReadTokens, turn.CacheWriteTokens, turn.ReasoningTokens, turn.ReasoningContent, boolInt(turn.InputIncludesCache), turn.DurationMS, turn.GenerationMS, turn.Block, now)
		if err != nil {
			return saved, err
		}
		saved = append(saved, StoredTurn(id, sq, now, turn))
	}
	if err := tx.Commit(); err != nil {
		return saved, err
	}
	_, _ = s.db.Exec(`UPDATE chat_sessions SET updated_at=? WHERE id=?`, time.Now().Unix(), id)
	return saved, nil
}

func (s *SQLite) Unfinished(limit int) ([]UnfinishedTurn, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT session_id, seq, text, created_at FROM chat_turns WHERE role='user' ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnfinishedTurn
	for rows.Next() {
		var u UnfinishedTurn
		if err := rows.Scan(&u.SessionID, &u.Seq, &u.Text, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }

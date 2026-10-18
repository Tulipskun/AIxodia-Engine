package session

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ai-engine/provider"
)

// SQLite serves chat history out of the engine's own store. The file and its
// schema belong to localdb, which opens it once and hands the handle around.
type SQLite struct {
	db *sql.DB
}

func NewSQLite(handle *sql.DB) *SQLite {
	return &SQLite{db: handle}
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

// ErrNotFound is what a chat that is not in the store returns, so a route can
// tell "no such chat" from "the store failed".
var ErrNotFound = errors.New("session not found")

func (s *SQLite) Get(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions WHERE id=?`, id)
	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
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
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scanner serves
// Get and List. They used to be two copies, and only one of them was ever run.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner) (*Session, error) {
	var sess Session
	var subEnabled, reasoning int
	var temp, topP sql.NullFloat64
	var maxTokens sql.NullInt64
	err := row.Scan(&sess.ID, &sess.Title, &sess.Provider, &sess.Model, &sess.SubProvider, &sess.SubModel, &subEnabled, &reasoning, &temp, &topP, &maxTokens, &sess.CreatedAt, &sess.UpdatedAt)
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

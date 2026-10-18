package main

import (
	"log"
	"strings"

	"ai-engine/db"
	"ai-engine/session"
)

// Chat history lives on the engine machine, but the phone's older chats were
// written before that was true and still sit in D1's chat_sessions/chat_turns.
// This moves them over once, and only when the local store has no sessions at
// all, so it cannot overwrite or duplicate a store that is already in use.
// Once D1's chat tables are gone the read finds no table and does nothing.

const (
	legacySessionsSQL = `SELECT id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at FROM chat_sessions`
	legacyTurnsSQL    = `SELECT session_id,seq,role,agent,job_id,text,created_at,model,input_tokens,output_tokens,duration_ms,cache_read_tokens,cache_write_tokens,reasoning_tokens,input_includes_cache,generation_ms,reasoning_content,block FROM chat_turns ORDER BY session_id, seq`
)

func importLegacyHistory(store *session.SQLite) {
	local, err := store.List()
	if err != nil {
		log.Printf("import: cannot read the local store: %v", err)
		return
	}
	if len(local) > 0 {
		return
	}

	rows, err := db.Query(legacySessionsSQL)
	if err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			log.Printf("import: D1 chat_sessions unreadable: %v", err)
		}
		return
	}
	if len(rows) == 0 {
		return
	}

	turnRows, err := db.Query(legacyTurnsSQL)
	if err != nil {
		log.Printf("import: D1 chat_turns unreadable: %v", err)
		return
	}
	bySession := map[string][]session.Turn{}
	for _, row := range turnRows {
		turn := turnFromRow(row)
		bySession[turn.SessionID] = append(bySession[turn.SessionID], turn)
	}

	sessions, turns := 0, 0
	for _, row := range rows {
		sess := sessionFromRow(row)
		added, n, err := store.Import(sess, bySession[sess.ID])
		if err != nil {
			log.Printf("import: %s: %v", sess.ID, err)
			continue
		}
		if added {
			sessions++
		}
		turns += n
	}
	log.Printf("import: D1 history -> local store: %d sessions, %d turns", sessions, turns)
}

func sessionFromRow(row map[string]any) session.Session {
	return session.Session{
		ID:          rowString(row, "id"),
		Title:       rowString(row, "title"),
		Provider:    rowString(row, "provider"),
		Model:       rowString(row, "model"),
		SubProvider: rowString(row, "sub_provider"),
		SubModel:    rowString(row, "sub_model"),
		SubEnabled:  rowInt(row, "sub_enabled") != 0,
		Reasoning:   rowInt(row, "reasoning") != 0,
		Temperature: rowFloat(row, "temperature"),
		TopP:        rowFloat(row, "top_p"),
		MaxTokens:   rowInt(row, "max_tokens"),
		CreatedAt:   rowInt64(row, "created_at"),
		UpdatedAt:   rowInt64(row, "updated_at"),
	}
}

func turnFromRow(row map[string]any) session.Turn {
	return session.Turn{
		SessionID:          rowString(row, "session_id"),
		Seq:                rowInt(row, "seq"),
		Role:               rowString(row, "role"),
		Agent:              rowString(row, "agent"),
		JobID:              rowString(row, "job_id"),
		Text:               rowString(row, "text"),
		CreatedAt:          rowInt64(row, "created_at"),
		Model:              rowString(row, "model"),
		InputTokens:        rowInt(row, "input_tokens"),
		OutputTokens:       rowInt(row, "output_tokens"),
		DurationMS:         rowInt64(row, "duration_ms"),
		CacheReadTokens:    rowInt(row, "cache_read_tokens"),
		CacheWriteTokens:   rowInt(row, "cache_write_tokens"),
		ReasoningTokens:    rowInt(row, "reasoning_tokens"),
		InputIncludesCache: rowInt(row, "input_includes_cache") != 0,
		GenerationMS:       rowInt64(row, "generation_ms"),
		ReasoningContent:   rowString(row, "reasoning_content"),
		Block:              rowString(row, "block"),
	}
}

// D1 hands every number back as a JSON number, i.e. a float64, and a missing
// or NULL column as nil.

func rowString(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

func rowInt(row map[string]any, key string) int {
	f, _ := row[key].(float64)
	return int(f)
}

func rowInt64(row map[string]any, key string) int64 {
	f, _ := row[key].(float64)
	return int64(f)
}

func rowFloat(row map[string]any, key string) *float64 {
	if f, ok := row[key].(float64); ok {
		return &f
	}
	return nil
}

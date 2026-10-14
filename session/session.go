package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"ai-engine/db"
	"ai-engine/provider"
)

type Session struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	SubProvider string   `json:"sub_provider,omitempty"`
	SubModel    string   `json:"sub_model,omitempty"`
	SubEnabled  bool     `json:"sub_enabled,omitempty"`
	Reasoning   bool     `json:"reasoning,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

type Turn struct {
	SessionID          string `json:"session_id"`
	Seq                int    `json:"seq"`
	Role               string `json:"role"`
	Agent              string `json:"agent,omitempty"`
	JobID              string `json:"job_id,omitempty"`
	Text               string `json:"text"`
	Model              string `json:"model,omitempty"`
	InputTokens        int    `json:"input_tokens,omitempty"`
	OutputTokens       int    `json:"output_tokens,omitempty"`
	CacheReadTokens    int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int    `json:"cache_write_tokens,omitempty"`
	ReasoningTokens    int    `json:"reasoning_tokens,omitempty"`
	ReasoningContent   string `json:"reasoning_content,omitempty"`
	InputIncludesCache bool   `json:"input_includes_cache,omitempty"`
	DurationMS         int64  `json:"duration_ms,omitempty"`
	GenerationMS       int64  `json:"generation_ms,omitempty"`
	CreatedAt          int64  `json:"created_at"`
}

type NewSession struct {
	Title       string
	Provider    string
	Model       string
	SubProvider string
	SubModel    string
	SubEnabled  bool
	Reasoning   bool
	Temperature *float64
	TopP        *float64
	MaxTokens   int
}

type TurnInput struct {
	Role         string
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	// Cache and reasoning counts, and the flag that says whether InputTokens
	// already contains the cache read.
	CacheReadTokens    int
	CacheWriteTokens   int
	ReasoningTokens    int
	InputIncludesCache bool
	// ReasoningContent travels with an assistant turn so the next request can
	// replay it; providers that serve a reasoning model refuse a history whose
	// assistant message leaves it out.
	ReasoningContent string
	// DurationMS is the whole turn, from the question being sent to the answer
	// being stored, which is the number a reader expects to see. GenerationMS is
	// only the time the model spent writing, and it is what a rate is computed
	// from: the two answer different questions.
	DurationMS   int64
	GenerationMS int64
}

type D1 struct{}

func NewD1() *D1 { return &D1{} }

func (d *D1) Create(params NewSession) (*Session, error) {
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

	row := map[string]any{
		"id":           session.ID,
		"title":        session.Title,
		"provider":     session.Provider,
		"model":        session.Model,
		"sub_provider": session.SubProvider,
		"sub_model":    session.SubModel,
		"sub_enabled":  boolInt(session.SubEnabled),
		"reasoning":    boolInt(session.Reasoning),
		"max_tokens":   session.MaxTokens,
		"created_at":   session.CreatedAt,
		"updated_at":   session.UpdatedAt,
	}
	if session.Temperature != nil {
		row["temperature"] = *session.Temperature
	}
	if session.TopP != nil {
		row["top_p"] = *session.TopP
	}
	if _, err := db.Insert("sessions", row); err != nil {
		return nil, err
	}
	return &session, nil
}

func (d *D1) Get(id string) (*Session, error) {
	rows, err := db.Select("sessions", db.Where{"id": id})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return sessionFromRow(rows[0]), nil
}

func (d *D1) List() ([]Session, error) {
	rows, err := db.Query("SELECT * FROM sessions ORDER BY updated_at DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, *sessionFromRow(row))
	}
	return sessions, nil
}

func (d *D1) Delete(id string) error {
	if err := db.Delete("turns", db.Where{"session_id": id}); err != nil {
		return err
	}
	return db.Delete("sessions", db.Where{"id": id})
}

func (d *D1) SaveConfig(id string, session Session) error {
	data := map[string]any{
		"provider":   session.Provider,
		"model":      session.Model,
		"updated_at": time.Now().Unix(),
	}
	if session.Title != "" {
		data["title"] = session.Title
	}
	_, err := db.Update("sessions", data, db.Where{"id": id})
	return err
}

func (d *D1) Turns(id string) ([]Turn, error) {
	rows, err := db.Query("SELECT * FROM turns WHERE session_id = ? ORDER BY seq ASC", id)
	if err != nil {
		return nil, err
	}
	turns := make([]Turn, 0, len(rows))
	for _, row := range rows {
		turns = append(turns, Turn{
			SessionID:          stringField(row, "session_id"),
			Seq:                intField(row, "seq"),
			Role:               stringField(row, "role"),
			Agent:              stringField(row, "agent"),
			JobID:              stringField(row, "job_id"),
			Text:               stringField(row, "text"),
			Model:              stringField(row, "model"),
			InputTokens:        intField(row, "input_tokens"),
			OutputTokens:       intField(row, "output_tokens"),
			CacheReadTokens:    intField(row, "cache_read_tokens"),
			CacheWriteTokens:   intField(row, "cache_write_tokens"),
			ReasoningTokens:    intField(row, "reasoning_tokens"),
			ReasoningContent:   stringField(row, "reasoning_content"),
			InputIncludesCache: intField(row, "input_includes_cache") != 0,
			DurationMS:         int64Field(row, "duration_ms"),
			GenerationMS:       int64Field(row, "generation_ms"),
			CreatedAt:          int64Field(row, "created_at"),
		})
	}
	return turns, nil
}

func (d *D1) History(id string) ([]provider.Message, error) {
	turns, err := d.Turns(id)
	if err != nil {
		return nil, err
	}
	return MessagesFromTurns(turns), nil
}

// const maxTurnsPerStatement keeps each INSERT inside D1's per-statement
// bound-parameter limit (100 parameters; 15 columns => 6 rows is the safe
// whole number below the line), so Appending N turns costs O(N/6) HTTP
// round-trips instead of N.
const maxTurnsPerStatement = 6

func (d *D1) AppendTurns(id string, turns []TurnInput) error {
	if len(turns) == 0 {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		seq, err := d.nextSeq(id)
		if err != nil {
			return err
		}

		now := time.Now().Unix()
		lastErr = nil
		for start := 0; start < len(turns); start += maxTurnsPerStatement {
			end := start + maxTurnsPerStatement
			if end > len(turns) {
				end = len(turns)
			}
			chunk := turns[start:end]

			placeholders := make([]string, 0, len(chunk))
			values := make([]any, 0, len(chunk)*15)
			for i, turn := range chunk {
				placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				values = append(values,
					id,
					seq+start+i,
					turn.Role,
					turn.Text,
					turn.Model,
					turn.InputTokens,
					turn.OutputTokens,
					turn.CacheReadTokens,
					turn.CacheWriteTokens,
					turn.ReasoningTokens,
					turn.ReasoningContent,
					boolInt(turn.InputIncludesCache),
					turn.DurationMS,
					turn.GenerationMS,
					now,
				)
			}
			sql := "INSERT INTO turns (session_id, seq, role, text, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, reasoning_content, input_includes_cache, duration_ms, generation_ms, created_at) VALUES " + strings.Join(placeholders, ", ")
			if _, err := db.Query(sql, values...); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr != nil {
			continue
		}
		return d.touch(id)
	}
	return lastErr
}

func (d *D1) nextSeq(id string) (int, error) {
	rows, err := db.Query("SELECT COALESCE(MAX(seq), 0) AS seq FROM turns WHERE session_id = ?", id)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 1, nil
	}
	return intField(rows[0], "seq") + 1, nil
}

func (d *D1) touch(id string) error {
	_, err := db.Update("sessions", map[string]any{"updated_at": time.Now().Unix()}, db.Where{"id": id})
	return err
}

func sessionFromRow(row map[string]any) *Session {
	return &Session{
		ID:          stringField(row, "id"),
		Title:       stringField(row, "title"),
		Provider:    stringField(row, "provider"),
		Model:       stringField(row, "model"),
		SubProvider: stringField(row, "sub_provider"),
		SubModel:    stringField(row, "sub_model"),
		SubEnabled:  boolField(row, "sub_enabled"),
		Reasoning:   boolField(row, "reasoning"),
		Temperature: floatField(row, "temperature"),
		TopP:        floatField(row, "top_p"),
		MaxTokens:   intField(row, "max_tokens"),
		CreatedAt:   int64Field(row, "created_at"),
		UpdatedAt:   int64Field(row, "updated_at"),
	}
}

func newID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func stringField(row map[string]any, key string) string {
	switch value := row[key].(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func intField(row map[string]any, key string) int {
	return int(int64Field(row, key))
}

func int64Field(row map[string]any, key string) int64 {
	switch value := row[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case string:
		var parsed int64
		fmt.Sscanf(value, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

func floatField(row map[string]any, key string) *float64 {
	switch value := row[key].(type) {
	case float64:
		return &value
	case int:
		converted := float64(value)
		return &converted
	case nil:
		return nil
	default:
		return nil
	}
}

func boolField(row map[string]any, key string) bool {
	switch value := row[key].(type) {
	case float64:
		return value != 0
	case bool:
		return value
	case string:
		return value == "1" || value == "true"
	default:
		return false
	}
}

// EnsureTurns has the columns the current engine writes. Adding a column is
// cheap and idempotent, and doing it here means a database that predates the
// generation clock keeps working instead of failing every insert.
func EnsureTurns() {
	// A duplicate column is the expected answer on every boot after the first,
	// so it is not worth a line in the log; anything else is.
	if _, err := db.Query("ALTER TABLE turns ADD COLUMN generation_ms INTEGER NOT NULL DEFAULT 0"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		log.Printf("session: cannot add generation_ms: %v", err)
	}
	// reasoning_content holds what a reasoning model wrote before its answer. It
	// is what the next request replays on that assistant turn; without it a
	// provider that serves one refuses the whole conversation with a
	// missing-parameter 400.
	if _, err := db.Query("ALTER TABLE turns ADD COLUMN reasoning_content TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		log.Printf("session: cannot add reasoning_content: %v", err)
	}
}

// UnfinishedTurn is a question that was stored and never answered — which is
// exactly what a turn interrupted by the session ending looks like. The phone's
// message is written before the answer is asked for, so a restart leaves one of
// these behind instead of losing the message.
type UnfinishedTurn struct {
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

// Unfinished lists the questions with nothing after them, newest first. It is
// what a new session reads to know what the old one was in the middle of.
//
// Only user and assistant rows are considered: a turn that ended mid tool call
// has its question answered in the next round, so the tail of a healthy
// conversation is an assistant row and not one of these.
func (d *D1) Unfinished(limit int) ([]UnfinishedTurn, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT t.session_id, t.seq, t.text, t.created_at
		   FROM turns t
		   JOIN (SELECT session_id, MAX(seq) AS seq FROM turns
		          WHERE role IN ('user','assistant') GROUP BY session_id) last
		     ON last.session_id = t.session_id AND last.seq = t.seq
		  WHERE t.role = 'user'
		  ORDER BY t.created_at DESC
		  LIMIT ` + strconv.Itoa(limit),
	)
	if err != nil {
		return nil, err
	}
	out := make([]UnfinishedTurn, 0, len(rows))
	for _, row := range rows {
		out = append(out, UnfinishedTurn{
			SessionID: stringField(row, "session_id"),
			Seq:       intField(row, "seq"),
			Text:      truncate(stringField(row, "text"), 200),
			CreatedAt: int64Field(row, "created_at"),
		})
	}
	return out, nil
}

// truncate shortens a question for a handover record, which only has to be
// recognisable, not complete.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "…"
}

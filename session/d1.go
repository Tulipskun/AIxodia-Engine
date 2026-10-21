package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"aixodia-engine/db"
	"aixodia-engine/provider"
)

// D1 is the chat store: sessions and their turns live in Cloudflare D1, so a
// fresh engine — and the phone — find the same history. D1 is reached over its
// HTTP query API; there is no local database in the engine any more.
type D1 struct {
	// Appends serialise within the process: nextSeq is a read followed by an
	// insert, and two answers for one chat that overlapped could pick the same
	// seq. One INSERT then failed and its retry put the answer after a newer
	// question, which the phone showed as a thread out of order.
	appendMu sync.Mutex
}

func NewD1() *D1 { return &D1{} }

// ErrNotFound is what a chat that is not in the store returns, so a route can
// tell "no such chat" from "the store failed".
var ErrNotFound = errors.New("session not found")

func (d *D1) Create(params NewSession) (*Session, error) {
	now := time.Now().Unix()
	id := strings.TrimSpace(params.ID)
	if id == "" {
		id = newID()
	}
	session := Session{
		ID:          id,
		Title:       params.Title,
		Provider:    params.Provider,
		Model:       params.Model,
		Reasoning:   params.Reasoning,
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   params.MaxTokens,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	_, err := db.Query(
		// The sub_provider/sub_model/sub_enabled columns still exist in the
		// external D1 schema. Write inert values so old rows and readers keep
		// working while nothing in the engine selects or uses them any more.
		`INSERT INTO chat_sessions (id,title,provider,model,sub_provider,sub_model,sub_enabled,reasoning,temperature,top_p,max_tokens,created_at,updated_at)
		 VALUES (?,?,?,?,'','',0,?,?,?,?,?,?)`,
		session.ID, session.Title, session.Provider, session.Model,
		boolInt(session.Reasoning), session.Temperature, session.TopP,
		session.MaxTokens, session.CreatedAt, session.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

const sessionColumns = `id,title,provider,model,reasoning,temperature,top_p,max_tokens,created_at,updated_at`

func (d *D1) Get(id string) (*Session, error) {
	rows, err := db.Query(`SELECT `+sessionColumns+` FROM chat_sessions WHERE id=? LIMIT 1`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return sessionFromRow(rows[0]), nil
}

func (d *D1) List() ([]Session, error) {
	// updated_at is whole seconds, so many chats can share a timestamp. The id
	// tiebreaker is the same one Room uses; without it the list order was left
	// to D1 and could change between identical reads.
	rows, err := db.Query(`SELECT ` + sessionColumns + ` FROM chat_sessions ORDER BY updated_at DESC, id DESC LIMIT 200`)
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
	if _, err := db.Query(`DELETE FROM chat_turns WHERE session_id=?`, id); err != nil {
		return err
	}
	_, err := db.Query(`DELETE FROM chat_sessions WHERE id=?`, id)
	return err
}

func (d *D1) SaveConfig(id string, session Session) error {
	rows, err := db.Query(
		`UPDATE chat_sessions SET provider=?, model=?, title=CASE WHEN ? != '' THEN ? ELSE title END, updated_at=? WHERE id=? RETURNING id`,
		session.Provider, session.Model, session.Title, session.Title, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}

// SaveAgent writes a chat's pinned agent choice and moves updated_at with it:
// the chat list is ordered by that column, and pinning a model is a change to
// the chat. The title and the model the chat itself runs on are left alone —
// the chat path writes those when a reply succeeds, and a pin that blanked them
// would rename the chat on the phone.
func (d *D1) SaveAgent(id string, patch AgentPatch) error {
	columns := []string{}
	values := []any{}
	add := func(column string, value any) {
		columns = append(columns, column+"=?")
		values = append(values, value)
	}
	if patch.Provider != nil {
		add("provider", *patch.Provider)
	}
	if patch.Model != nil {
		add("model", *patch.Model)
	}
	if len(columns) == 0 {
		return nil
	}
	add("updated_at", time.Now().Unix())
	values = append(values, id)

	// D1's HTTP API reports no affected-row count, so existence is checked
	// first: a patch to a chat that is gone must read as 404, not as success.
	if _, err := d.Get(id); err != nil {
		return err
	}
	_, err := db.Query(`UPDATE chat_sessions SET `+strings.Join(columns, ", ")+` WHERE id=?`, values...)
	return err
}

const turnColumns = `session_id,seq,role,agent,job_id,text,created_at,model,input_tokens,output_tokens,duration_ms,cache_read_tokens,cache_write_tokens,reasoning_tokens,input_includes_cache,generation_ms,reasoning_content,block`

func (d *D1) Turns(id string) ([]Turn, error) {
	rows, err := db.Query(`SELECT `+turnColumns+` FROM chat_turns WHERE session_id=? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, err
	}
	turns := make([]Turn, 0, len(rows))
	for _, row := range rows {
		turns = append(turns, turnFromRow(row))
	}
	return turns, nil
}

// TurnsPage is the newest page ending just before beforeSeq, returned
// oldest-first like Turns. beforeSeq <= 0 means the newest page. The phone
// reads one page per scroll, so a long transcript no longer travels over the
// tunnel whole because its first screen needed the last sixty rows.
func (d *D1) TurnsPage(id string, beforeSeq int64, limit int) ([]Turn, error) {
	if limit <= 0 {
		return d.Turns(id)
	}
	var (
		rows []map[string]any
		err  error
	)
	if beforeSeq > 0 {
		rows, err = db.Query(`SELECT `+turnColumns+` FROM chat_turns WHERE session_id=? AND seq<? ORDER BY seq DESC LIMIT ?`, id, beforeSeq, limit)
	} else {
		rows, err = db.Query(`SELECT `+turnColumns+` FROM chat_turns WHERE session_id=? ORDER BY seq DESC LIMIT ?`, id, limit)
	}
	if err != nil {
		return nil, err
	}
	turns := make([]Turn, 0, len(rows))
	// SQL walked backwards from beforeSeq so LIMIT took the page nearest it;
	// callers (and the screen) read a transcript oldest-first.
	for i := len(rows) - 1; i >= 0; i-- {
		turns = append(turns, turnFromRow(rows[i]))
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

// maxTurnsPerStatement keeps each INSERT inside D1's per-statement
// bound-parameter limit (100 parameters; 16 columns => 6 rows is 96, the safe
// whole number below the line), so appending N turns costs O(N/6) HTTP
// round-trips instead of N.
const maxTurnsPerStatement = 6

// AppendTurns writes the turns of one round. It retries only the unwritten
// suffix: replaying a successful chunk used to duplicate it when a later INSERT
// failed.
func (d *D1) AppendTurns(id string, turns []TurnInput) ([]Turn, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	d.appendMu.Lock()
	defer d.appendMu.Unlock()
	var saved []Turn
	var lastErr error
	for attempt := 0; attempt < 3 && len(saved) < len(turns); attempt++ {
		seq, err := d.nextSeq(id)
		if err != nil {
			return saved, err
		}

		now := time.Now().Unix()
		base := len(saved)
		lastErr = nil
		for start := base; start < len(turns); start += maxTurnsPerStatement {
			end := start + maxTurnsPerStatement
			if end > len(turns) {
				end = len(turns)
			}
			chunk := turns[start:end]

			placeholders := make([]string, 0, len(chunk))
			values := make([]any, 0, len(chunk)*16)
			for i, turn := range chunk {
				placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
				values = append(values,
					id,
					seq+start-base+i,
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
					turn.Block,
					now,
				)
			}
			statement := "INSERT INTO chat_turns (session_id, seq, role, text, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, reasoning_content, input_includes_cache, duration_ms, generation_ms, block, created_at) VALUES " + strings.Join(placeholders, ", ")
			if _, err := db.Query(statement, values...); err != nil {
				lastErr = err
				break
			}
			for i, turn := range chunk {
				saved = append(saved, StoredTurn(id, seq+start-base+i, now, turn))
			}
		}
		if lastErr != nil {
			continue
		}
		// The rows are committed even if updating the session timestamp fails.
		if err := d.touch(id); err != nil {
			log.Printf("session: touch after append: %v", err)
		}
		return saved, nil
	}
	return saved, lastErr
}

func (d *D1) nextSeq(id string) (int, error) {
	rows, err := db.Query(`SELECT COALESCE(MAX(seq), 0) AS seq FROM chat_turns WHERE session_id=?`, id)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 1, nil
	}
	return intField(rows[0], "seq") + 1, nil
}

func (d *D1) touch(id string) error {
	_, err := db.Query(`UPDATE chat_sessions SET updated_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
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
		   FROM chat_turns t
		   JOIN (SELECT session_id, MAX(seq) AS seq FROM chat_turns
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
			Text:      truncateText(stringField(row, "text"), 200),
			CreatedAt: int64Field(row, "created_at"),
		})
	}
	return out, nil
}

// EnsureTurns has the columns the current engine writes. The D1 tables predate
// some of them, and adding a column is cheap and idempotent, so doing it here
// keeps an older database working instead of failing every insert.
func EnsureTurns() {
	for _, statement := range []string{
		`ALTER TABLE chat_turns ADD COLUMN generation_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_turns ADD COLUMN reasoning_content TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_turns ADD COLUMN block TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Query(statement); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			log.Printf("session: schema: %v", err)
		}
	}
}

// truncateText shortens a question for a handover record, which only has to be
// recognisable, not complete.
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "…"
}

func sessionFromRow(row map[string]any) *Session {
	return &Session{
		ID:          stringField(row, "id"),
		Title:       stringField(row, "title"),
		Provider:    stringField(row, "provider"),
		Model:       stringField(row, "model"),
		Reasoning:   boolField(row, "reasoning"),
		Temperature: floatField(row, "temperature"),
		TopP:        floatField(row, "top_p"),
		MaxTokens:   intField(row, "max_tokens"),
		CreatedAt:   int64Field(row, "created_at"),
		UpdatedAt:   int64Field(row, "updated_at"),
	}
}

func turnFromRow(row map[string]any) Turn {
	return Turn{
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
		// A database that predates the block column has no such key, which
		// reads as blank — the same as a row that predates blocks.
		Block:     stringField(row, "block"),
		CreatedAt: int64Field(row, "created_at"),
	}
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

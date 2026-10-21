package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// The engine's chat vocabulary. The store that holds it is D1 (d1.go), so the
// phone and a fresh engine see the same conversations.

type Session struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Reasoning   bool     `json:"reasoning,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	// Usage is the chat's cost, summed by the store over every turn. It rides
	// with the session so a phone that only holds the newest page of rows can
	// still draw the whole chat's figures without loading the rest. The phone
	// draws the strip from these figures and keeps its own copy in session
	// metadata; it never adds a page of turns up itself.
	Usage *SessionUsage `json:"usage,omitempty"`
}

// SessionUsage is one chat's cost. The two prompt conventions stay apart: a
// provider that puts the cache inside its input total must not have that cache
// added on top of it, so PromptTokens is the whole prompt either way while
// FreshInput is only the part that was not served from cache.
type SessionUsage struct {
	PromptTokens int   `json:"prompt_tokens"`
	CacheRead    int   `json:"cache_read"`
	FreshInput   int   `json:"fresh_input"`
	OutputTokens int   `json:"output_tokens"`
	TotalMS      int64 `json:"total_ms"`
	// LastTurnMS is the newest answer that reported a duration, not the
	// longest: the strip calls it "last turn", not an average.
	LastTurnMS int64 `json:"last_turn_ms"`
}

type Turn struct {
	SessionID string `json:"session_id"`
	// ID is the D1 row id: a number that only moves forward, so it is unique
	// for every message and easy for the phone to use as a key. Seq stays the
	// per-session order and the cursor history pages are read with.
	ID                 int64  `json:"id"`
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
	// Block is how this row draws: the engine's display vocabulary, so a new
	// kind of content needs no app update. Empty on rows that predate blocks,
	// which the phone reads through the role instead.
	Block     string `json:"block,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type NewSession struct {
	// ID is optional. The phone makes a draft chat locally and only asks the
	// daemon to create it when the first message is sent; keeping that id means
	// the draft does not have to be migrated to a new server-generated one.
	// An empty ID still gets the engine's own random id.
	ID          string
	Title       string
	Provider    string
	Model       string
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
	// Block is the row's display block as JSON. Replay ignores it — the role
	// and text still carry the conversation — so it only ever changes looks.
	Block string
}

// StoredTurn describes the exact row inserted, without another history query.
func StoredTurn(id string, seq int, createdAt int64, input TurnInput) Turn {
	return Turn{
		SessionID: id, Seq: seq, CreatedAt: createdAt, Role: input.Role,
		Text: input.Text, Model: input.Model, InputTokens: input.InputTokens,
		OutputTokens: input.OutputTokens, CacheReadTokens: input.CacheReadTokens,
		CacheWriteTokens: input.CacheWriteTokens, ReasoningTokens: input.ReasoningTokens,
		ReasoningContent: input.ReasoningContent, InputIncludesCache: input.InputIncludesCache,
		DurationMS: input.DurationMS, GenerationMS: input.GenerationMS,
		Block: input.Block,
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

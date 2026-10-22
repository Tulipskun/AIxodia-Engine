// Package run owns the long-lived agent turns the phone starts with
// POST /v1/runs. A run outlives the request that created it: its events are
// numbered, kept for replay and streamed to every viewer that attaches later.
package run

import (
	"encoding/json"
	"time"
)

// Status is the lifecycle of a run. waiting_approval is written before a tool
// that needs a human decision is executed, so a restart leaves a run that can
// be answered instead of one that silently continued.
type Status string

const (
	StatusRunning         Status = "running"
	StatusWaitingApproval Status = "waiting_approval"
	StatusDone            Status = "done"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusInterrupted     Status = "interrupted"
)

// Terminal reports whether no further events will arrive.
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Event kinds. The phone only needs the kind to choose a renderer; Data is the
// payload the gateway already speaks (a turn row, a tool_event, a delta...).
const (
	KindStatus    = "status"
	KindUser      = "user"
	KindAssistant = "assistant"
	KindReasoning = "reasoning"
	KindTool      = "tool"
	KindTurn      = "turn"
	KindFinal     = "final"
	KindError     = "error"
	KindApproval  = "approval"
)

// Event is one numbered entry in a run's log. Seq is per run and starts at 1.
type Event struct {
	// runID is not part of the wire shape; the store needs it to key a batch.
	runID string

	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	At   int64           `json:"at"`
	Data json.RawMessage `json:"data"`
}

// Run is the metadata a client polls or resumes from.
type Run struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ClientRunID string `json:"client_run_id,omitempty"`
	Status      Status `json:"status"`
	Error       string `json:"error,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	LastSeq     int64  `json:"last_seq"`
}

func unixNow() int64 { return time.Now().Unix() }

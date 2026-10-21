// Package goals keeps the durable list of goals and plans the model records
// with the objective tools. Goals and plans are separate lists: a goal is the
// outcome the user asked for, a plan is a step toward it. Both live in D1
// beside chat history, scoped to the chat session that created them.
package goals

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"aixodia-engine/db"
)

// Kind separates the two lists. They use the same storage and tool shape but
// never appear in each other's list.
type Kind string

const (
	Goal Kind = "goal"
	Plan Kind = "plan"

	StatusOpen    = "open"
	StatusDone    = "done"
	StatusDropped = "dropped"
)

// ErrNotFound is returned for an id that does not exist in the requested
// session and kind, so a tool can say "no such goal" without guessing from an
// empty update.
var ErrNotFound = errors.New("objective not found")

// Item is one open goal or plan.
type Item struct {
	ID      string
	Kind    Kind
	Content string
}

// Store is what the tools and the reminder builder need. D1 is the production
// implementation; tests use a map.
type Store interface {
	Add(sessionID string, kind Kind, content string) (id string, err error)
	Edit(sessionID string, kind Kind, id, content string) error
	Complete(sessionID string, kind Kind, id string) error
	Drop(sessionID string, kind Kind, id string) error
	// Open returns the goals and plans of one chat that are still active.
	// Completed and dropped rows are kept for audit but are not returned.
	Open(sessionID string) ([]Item, error)
}

// D1 is the goal and plan store.
type D1 struct{}

func NewD1() *D1 { return &D1{} }

// Ensure creates the objectives table. It runs at startup, like the chat
// schema checks; an existing table is left untouched.
func Ensure() {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS objectives (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'open',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS objectives_session ON objectives (session_id, kind, status)`,
	} {
		if _, err := db.Query(statement); err != nil {
			log.Printf("goals: schema: %v", err)
		}
	}
}

func (d *D1) Add(sessionID string, kind Kind, content string) (string, error) {
	sessionID, err := normalizeSession(sessionID)
	if err != nil {
		return "", err
	}
	if err := normalizeKind(kind); err != nil {
		return "", err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("%s content must not be empty", kind)
	}
	id := newID(kind)
	now := time.Now().Unix()
	_, err = db.Query(
		`INSERT INTO objectives (id, session_id, kind, content, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, string(kind), content, StatusOpen, now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Edit replaces the text of an open goal or plan. A completed or dropped item
// is not silently reopened by an edit.
func (d *D1) Edit(sessionID string, kind Kind, id, content string) error {
	sessionID, err := normalizeSession(sessionID)
	if err != nil {
		return err
	}
	if err := normalizeKind(kind); err != nil {
		return err
	}
	id, err = normalizeID(id, kind)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("%s content must not be empty", kind)
	}
	rows, err := db.Query(
		`UPDATE objectives SET content=?, updated_at=?
		 WHERE id=? AND session_id=? AND kind=? AND status=? RETURNING id`,
		content, time.Now().Unix(), id, sessionID, string(kind), StatusOpen)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return notOpen(sessionID, kind, id)
	}
	return nil
}

// Complete marks a goal or plan done. Completing one that is already done is
// harmless; a dropped item cannot be completed. Either way it disappears from
// the active list, like a dropped item.
func (d *D1) Complete(sessionID string, kind Kind, id string) error {
	sessionID, err := normalizeSession(sessionID)
	if err != nil {
		return err
	}
	if err := normalizeKind(kind); err != nil {
		return err
	}
	id, err = normalizeID(id, kind)
	if err != nil {
		return err
	}
	rows, err := db.Query(
		`UPDATE objectives SET status=?, updated_at=?
		 WHERE id=? AND session_id=? AND kind=? AND status!=? RETURNING id`,
		StatusDone, time.Now().Unix(), id, sessionID, string(kind), StatusDropped)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		status, err := statusOf(sessionID, kind, id)
		if err != nil {
			return err
		}
		if status == StatusDropped {
			return fmt.Errorf("%s %s was dropped and cannot be completed", kind, id)
		}
	}
	return nil
}

// Drop marks a goal or plan dropped. It is how a user cancels work that should
// no longer be pursued.
func (d *D1) Drop(sessionID string, kind Kind, id string) error {
	sessionID, err := normalizeSession(sessionID)
	if err != nil {
		return err
	}
	if err := normalizeKind(kind); err != nil {
		return err
	}
	id, err = normalizeID(id, kind)
	if err != nil {
		return err
	}
	rows, err := db.Query(
		`UPDATE objectives SET status=?, updated_at=?
		 WHERE id=? AND session_id=? AND kind=? RETURNING id`,
		StatusDropped, time.Now().Unix(), id, sessionID, string(kind))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}

// Open returns the active goals and plans of one chat, oldest first, so the
// model sees them in the order the conversation created them.
func (d *D1) Open(sessionID string) ([]Item, error) {
	sessionID, err := normalizeSession(sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id, kind, content FROM objectives
		 WHERE session_id=? AND status=? ORDER BY created_at ASC, id ASC`,
		sessionID, StatusOpen)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		kind, _ := row["kind"].(string)
		content, _ := row["content"].(string)
		items = append(items, Item{ID: id, Kind: Kind(kind), Content: content})
	}
	return items, nil
}

func notOpen(sessionID string, kind Kind, id string) error {
	status, err := statusOf(sessionID, kind, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s %s is %s, not open", kind, id, status)
}

// statusOf returns an item's status, or ErrNotFound when the id is unknown in
// this session and kind.
func statusOf(sessionID string, kind Kind, id string) (string, error) {
	rows, err := db.Query(
		`SELECT status FROM objectives WHERE id=? AND session_id=? AND kind=? LIMIT 1`,
		id, sessionID, string(kind))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	status, _ := rows[0]["status"].(string)
	return status, nil
}

func normalizeSession(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("goal and plan tools need an active chat session")
	}
	return sessionID, nil
}

func normalizeKind(kind Kind) error {
	if kind != Goal && kind != Plan {
		return fmt.Errorf("unknown objective kind %q", kind)
	}
	return nil
}

func normalizeID(id string, kind Kind) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("%s_id must not be empty", kind)
	}
	return id, nil
}

// newID is short enough to quote in a conversation and wide enough (48 random
// bits) that two items cannot reasonably collide. The prefix also makes a goal
// id and a plan id distinguishable at a glance.
func newID(kind Kind) string {
	prefix := "g-"
	if kind == Plan {
		prefix = "p-"
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

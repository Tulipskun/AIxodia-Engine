// Package goals keeps the small, durable list of objectives the model records
// with the goal tools. Goals live in D1 beside chat history so a restart — or a
// fresh engine on another machine — still knows what was being worked on.
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

const (
	StatusOpen    = "open"
	StatusDone    = "done"
	StatusDropped = "dropped"
)

// ErrNotFound is returned for a goal id that does not exist, so a tool can say
// "no such goal" without guessing from an empty update.
var ErrNotFound = errors.New("goal not found")

// D1 is the goal store.
type D1 struct{}

func NewD1() *D1 { return &D1{} }

// Ensure creates the goals table. It runs at startup, like the chat schema
// checks; an existing table is left untouched.
func Ensure() {
	_, err := db.Query(`CREATE TABLE IF NOT EXISTS goals (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'open',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		log.Printf("goals: schema: %v", err)
	}
}

func (d *D1) Add(content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("goal content must not be empty")
	}
	id := newID()
	now := time.Now().Unix()
	_, err := db.Query(
		`INSERT INTO goals (id, content, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, content, StatusOpen, now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Edit replaces the text of an open goal. A completed or dropped goal is not
// silently reopened by an edit.
func (d *D1) Edit(id, content string) error {
	id, err := normalizeID(id)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("goal content must not be empty")
	}
	rows, err := db.Query(
		`UPDATE goals SET content=?, updated_at=? WHERE id=? AND status=? RETURNING id`,
		content, time.Now().Unix(), id, StatusOpen)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return notOpen(id)
	}
	return nil
}

// Complete marks a goal done. Completing a goal that is already done is
// harmless; a dropped goal cannot be completed.
func (d *D1) Complete(id string) error {
	id, err := normalizeID(id)
	if err != nil {
		return err
	}
	rows, err := db.Query(
		`UPDATE goals SET status=?, updated_at=? WHERE id=? AND status!=? RETURNING id`,
		StatusDone, time.Now().Unix(), id, StatusDropped)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		status, err := statusOf(id)
		if err != nil {
			return err
		}
		if status == StatusDropped {
			return fmt.Errorf("goal %s was dropped and cannot be completed", id)
		}
	}
	return nil
}

// Drop marks a goal dropped. It is how a user cancels work that should no
// longer be pursued.
func (d *D1) Drop(id string) error {
	id, err := normalizeID(id)
	if err != nil {
		return err
	}
	rows, err := db.Query(
		`UPDATE goals SET status=?, updated_at=? WHERE id=? RETURNING id`,
		StatusDropped, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}

func notOpen(id string) error {
	status, err := statusOf(id)
	if err != nil {
		return err
	}
	return fmt.Errorf("goal %s is %s, not open", id, status)
}

// statusOf returns a goal's status, or ErrNotFound when the id is unknown.
func statusOf(id string) (string, error) {
	rows, err := db.Query(`SELECT status FROM goals WHERE id=? LIMIT 1`, id)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	status, _ := rows[0]["status"].(string)
	return status, nil
}

func normalizeID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("goal_id must not be empty")
	}
	return id, nil
}

// newID is short enough to quote in a conversation and wide enough (48 random
// bits) that two goals cannot reasonably collide.
func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("g-%d", time.Now().UnixNano())
	}
	return "g-" + hex.EncodeToString(b[:])
}

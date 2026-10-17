package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"ai-engine/db"
	"ai-engine/session"
)

// handoverStateKey is where a session leaves a note for the one that replaces
// it. The key already existed; nothing wrote it, so it is defined here.
const handoverStateKey = "daemon/ready"

// Handover is what a new session records about itself and about the session it
// replaced. A Kaggle kernel is killed at twelve hours whether or not it is in
// the middle of something, so the interesting question on boot is not "did I
// start" but "was the session before me cut off mid-turn, and what was it
// answering".
type Handover struct {
	// Instance identifies this process, so a note can be told apart from the one
	// the previous session left.
	Instance string `json:"instance"`
	Tunnel   string `json:"tunnel"`
	Version  string `json:"version"`
	// KernelStarted is when the notebook began, in unix seconds. It is what
	// uptime is measured from: the engine restarts itself on every update, so its
	// own start time is not the session's age. Zero when the notebook did not say.
	KernelStarted int64 `json:"kernel_started"`
	EngineStarted int64 `json:"engine_started"`
	At            int64 `json:"at"`
	// Unfinished are the questions the previous session stored and never
	// answered, newest first.
	Unfinished []session.UnfinishedTurn `json:"unfinished,omitempty"`
}

// publishHandover writes the note. It runs after the tunnel is published, so the
// record names a URL that is already reachable, and it never blocks startup: a
// note is useful, not load-bearing.
func publishHandover(ctx context.Context, tunnelURL string) {
	note := Handover{
		Instance:      "daemon-" + strconv.FormatInt(startedAt, 10),
		Tunnel:        tunnelURL,
		Version:       versionString(),
		KernelStarted: kernelStarted(),
		EngineStarted: startedAt,
		At:            time.Now().Unix(),
	}
	store := sessionStore
	if store == nil {
		store = session.NewD1()
	}
	if open, err := store.Unfinished(20); err == nil {
		note.Unfinished = open
	} else {
		log.Printf("handover: cannot list unfinished turns: %v", err)
	}

	payload, err := json.Marshal(note)
	if err != nil {
		log.Printf("handover: %v", err)
		return
	}
	if _, err := db.Query(
		"INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, unixepoch()) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		handoverStateKey, string(payload),
	); err != nil {
		log.Printf("handover: cannot write the note: %v", err)
		return
	}
	if n := len(note.Unfinished); n > 0 {
		log.Printf("handover: the previous session left %d question(s) unanswered", n)
		for _, turn := range note.Unfinished {
			log.Printf("handover:   %s seq %d: %s", turn.SessionID, turn.Seq, turn.Text)
		}
	}
	log.Printf("handover: note written (kernel age %s)", kernelAge())
}

// kernelStarted is when the notebook began, reported by the notebook through the
// environment. The engine cannot read it from anywhere else: the notebook is the
// only thing that knows, and the engine restarts itself without the session
// ending.
func kernelStarted() int64 {
	raw := strings.TrimSpace(os.Getenv("AIXODIA_KERNEL_STARTED"))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return seconds
}

func kernelAge() string {
	started := kernelStarted()
	if started <= 0 {
		return "unknown"
	}
	age := time.Since(time.Unix(started, 0))
	return age.Truncate(time.Minute).String()
}

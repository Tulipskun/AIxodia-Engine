package main

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"aixodia-engine/localdb"
	"aixodia-engine/session"
)

// handoverStateKey is where a session leaves a note for the one that replaces
// it. The key already existed; nothing wrote it, so it is defined here.
const handoverStateKey = "daemon/ready"

// Handover is what a new session records about itself and about the session it
// replaced. A session is killed at its fixed limit whether or not it is in the
// middle of something, so the interesting question on boot is not "did I
// start" but "was the session before me cut off mid-turn, and what was it
// answering".
type Handover struct {
	// Instance identifies this process, so a note can be told apart from the one
	// the previous session left.
	Instance string `json:"instance"`
	Tunnel   string `json:"tunnel"`
	Version  string `json:"version"`
	// KernelStarted is when this session began, in unix seconds. A session ends
	// when its engine ends, so the engine's own start is the session's start.
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
		KernelStarted: startedAt,
		EngineStarted: startedAt,
		At:            time.Now().Unix(),
	}
	store := sessionStore
	if store == nil {
		log.Print("handover: the session store is not open")
		return
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
	// The note is written to the engine's own store: there is no reader left on
	// the D1 side, and a note is only useful on this machine anyway.
	if err := localdb.PutState(storeDB, handoverStateKey, string(payload)); err != nil {
		log.Printf("handover: cannot write the note: %v", err)
		return
	}
	if n := len(note.Unfinished); n > 0 {
		log.Printf("handover: the previous session left %d question(s) unanswered", n)
		for _, turn := range note.Unfinished {
			log.Printf("handover:   %s seq %d: %s", turn.SessionID, turn.Seq, turn.Text)
		}
	}
	log.Printf("handover: note written (uptime %s)", time.Since(time.Unix(startedAt, 0)).Truncate(time.Minute))
}

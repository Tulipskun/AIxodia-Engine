package main

import (
	"ai-engine/io/mobile"
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestParseStarted(t *testing.T) {
	cases := map[string]int64{
		"ai-engine/1.1 started=1791457000": 1791457000,
		"ai-engine/1.0":                    0,
		"":                                 0,
		"ai-engine/1.1 started=abc":        0,
	}
	for in, want := range cases {
		if got := parseStarted(in); got != want {
			t.Errorf("parseStarted(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChatBridgeCarriesCommittedRowsAndFinalAnswer(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"pinned":false}`)
			return
		}
		fmt.Fprint(w, "data: "+`{"turns":[{"session_id":"s","seq":40,"role":"user","text":"hi"}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"tool_event":{"stage":"reasoning","text":"real reasoning"}}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"narration then answer"}}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"turns":[{"session_id":"s","seq":42,"role":"assistant","text":"answer","cache_read_tokens":80}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"turn":{"seq":42,"role":"assistant","text":"answer","cache_read_tokens":80,"reasoning_content":"real reasoning","generation_ms":200},"turns_synced":true}`+"\n\n")
	})
	var reasoning string
	got, err := chatViaGateway(h, "mock/m")(context.Background(), "s", "hi", nil, func(string) {}, func(ev mobile.ToolEvent) {
		if ev.Stage == "reasoning" {
			reasoning += ev.Text
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.TurnsSynced || got.Answer.Seq != 42 || got.Answer.Text != "answer" || len(got.Turns) != 2 || got.Answer.CacheReadTokens != 80 || reasoning != "real reasoning" {
		t.Fatalf("bad bridge result: %+v, reasoning=%q", got, reasoning)
	}
}

func TestSessionModelPrefersPin(t *testing.T) {
	pinned := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/s1" {
			w.Write([]byte(`{"provider":"Opencode","model":"big-pickle","pinned":true}`))
			return
		}
		w.Write([]byte(`{"provider":"","model":"","pinned":false}`))
	})
	if got := sessionModel(pinned, "s1", "Opencode/muse-spark"); got != "Opencode/big-pickle" {
		t.Errorf("pinned = %q", got)
	}
	if got := sessionModel(pinned, "s9", "Opencode/muse-spark"); got != "Opencode/muse-spark" {
		t.Errorf("unpinned = %q", got)
	}
	broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if got := sessionModel(broken, "s1", "Opencode/muse-spark"); got != "Opencode/muse-spark" {
		t.Errorf("error = %q", got)
	}
}

// A log helper must never take the daemon down when the log database has not
// been opened yet: main used to create the table before startD1Logging, which
// panicked the whole process on a nil handle.
func TestLogTableCreationIsSafeBeforeStart(t *testing.T) {
	saved := logDB
	logDB = nil
	defer func() { logDB = saved }()
	createEngineLogsTableLocal() // must not panic
}

// D1 answers with JSON: every number is a float64 and a missing column is nil.
// These two mappers are the whole of the legacy history import that does not
// need a live D1.
func TestLegacyRowsBecomeSessionsAndTurns(t *testing.T) {
	sess := sessionFromRow(map[string]any{
		"id": "abc123", "title": "old chat", "provider": "Opencode", "model": "mimo-v2.6-flash-free",
		"sub_enabled": float64(1), "temperature": 0.5, "max_tokens": float64(4096),
		"created_at": float64(1792246240), "updated_at": float64(1792246300),
	})
	if sess.ID != "abc123" || sess.Title != "old chat" || sess.Provider != "Opencode" || sess.Model != "mimo-v2.6-flash-free" {
		t.Fatalf("sessionFromRow = %+v", sess)
	}
	if !sess.SubEnabled || sess.Reasoning {
		t.Fatalf("sessionFromRow flags = sub %v reasoning %v, want true, false", sess.SubEnabled, sess.Reasoning)
	}
	if sess.Temperature == nil || *sess.Temperature != 0.5 || sess.MaxTokens != 4096 {
		t.Fatalf("sessionFromRow numbers = %+v", sess)
	}
	if sess.CreatedAt != 1792246240 || sess.UpdatedAt != 1792246300 {
		t.Fatalf("sessionFromRow timestamps = %+v", sess)
	}

	turn := turnFromRow(map[string]any{
		"session_id": "abc123", "seq": float64(3), "role": "assistant", "text": "hi",
		"input_tokens": float64(7), "duration_ms": float64(900), "input_includes_cache": float64(1),
	})
	if turn.SessionID != "abc123" || turn.Seq != 3 || turn.Role != "assistant" || turn.Text != "hi" {
		t.Fatalf("turnFromRow = %+v", turn)
	}
	if turn.InputTokens != 7 || turn.DurationMS != 900 || !turn.InputIncludesCache {
		t.Fatalf("turnFromRow numbers = %+v", turn)
	}
	if turn.Model != "" || turn.Block != "" || turn.CreatedAt != 0 {
		t.Fatalf("turnFromRow filled missing columns: %+v", turn)
	}
}

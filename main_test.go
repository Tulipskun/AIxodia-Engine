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

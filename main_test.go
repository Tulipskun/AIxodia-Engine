package main

import (
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

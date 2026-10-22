package run

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEventsEndpointReplaysAndResumes(t *testing.T) {
	store := newMemoryStore()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"turns\":[{\"seq\":1,\"role\":\"user\",\"text\":\"hi\"}]}\n\n")
		fmt.Fprint(w, "data: {\"turn\":{\"seq\":2,\"role\":\"assistant\",\"text\":\"hello\"},\"usage\":{\"prompt_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	m := New(handler, store)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"session_id":"s1","client_run_id":"api-1","message":"hi"}`)
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Run Run `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || created.Run.ID == "" {
		t.Fatalf("create status=%d run=%+v", resp.StatusCode, created.Run)
	}
	waitStatus(t, m, created.Run.ID, StatusDone)

	read := func(after int64) []Event {
		t.Helper()
		url := fmt.Sprintf("%s/v1/runs/%s/events?after=%d", srv.URL, created.Run.ID, after)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("content type = %q", ct)
		}
		var events []Event
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("event JSON: %v", err)
			}
			events = append(events, ev)
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return events
	}

	all := read(0)
	if len(all) < 4 {
		t.Fatalf("events = %+v", all)
	}
	for i, ev := range all {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, ev.Seq)
		}
	}
	if all[len(all)-1].Kind != KindStatus {
		t.Fatalf("last kind = %q", all[len(all)-1].Kind)
	}
	var last struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal(all[len(all)-1].Data, &last); err != nil || last.Status != StatusDone {
		t.Fatalf("last status = %+v err=%v", last, err)
	}

	resumed := read(3)
	if len(resumed) == 0 {
		t.Fatal("resume returned nothing")
	}
	for _, ev := range resumed {
		if ev.Seq <= 3 {
			t.Fatalf("resume repeated seq %d", ev.Seq)
		}
	}
}

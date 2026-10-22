package run

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunReplaysEveryEventInOrder(t *testing.T) {
	store := newMemoryStore()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"turns\":[{\"seq\":1,\"role\":\"user\",\"text\":\"hi\"}]}\n\n")
		fmt.Fprint(w, "data: {\"turn\":{\"seq\":2,\"role\":\"assistant\",\"text\":\"hello\"},\"usage\":{\"prompt_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	m := New(handler, store)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	created, existing, err := m.Create(StartRequest{SessionID: "s1", ClientRunID: "c1", Message: "hi"})
	if err != nil || existing {
		t.Fatalf("create: %v existing=%v", err, existing)
	}
	waitStatus(t, m, created.ID, StatusDone)

	meta, backlog, ch, cancel, active, err := m.Subscribe(created.ID, 0)
	if err != nil || active || ch != nil {
		t.Fatalf("subscribe: err=%v active=%v ch=%v", err, active, ch)
	}
	defer cancel()
	if meta.Status != StatusDone {
		t.Fatalf("status = %s", meta.Status)
	}
	if len(backlog) == 0 {
		t.Fatal("no events")
	}
	for i, ev := range backlog {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, ev.Seq)
		}
	}
	var assistant strings.Builder
	for _, ev := range backlog {
		if ev.Kind == KindAssistant {
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil {
				t.Fatal(err)
			}
			assistant.WriteString(payload.Text)
		}
	}
	if assistant.String() != "hello" {
		t.Fatalf("assistant = %q (events=%+v)", assistant.String(), backlog)
	}

	// A second create with the same idempotency key returns the same run and
	// does not start another turn.
	again, existing, err := m.Create(StartRequest{SessionID: "s1", ClientRunID: "c1", Message: "hi"})
	if err != nil || !existing || again.ID != created.ID {
		t.Fatalf("idempotency: %+v existing=%v err=%v", again, existing, err)
	}
}

func TestRunCancelStopsTheTurn(t *testing.T) {
	store := newMemoryStore()
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(started)
		<-r.Context().Done()
	})
	m := New(handler, store)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	created, _, err := m.Create(StartRequest{SessionID: "s1", ClientRunID: "c1", Message: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := m.Cancel(created.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, created.ID, StatusCancelled)
}

func TestSubscribeStartsAfterSeq(t *testing.T) {
	store := newMemoryStore()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	m := New(handler, store)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	created, _, err := m.Create(StartRequest{SessionID: "s1", ClientRunID: "c1", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, created.ID, StatusDone)
	_, backlog, _, _, _, err := m.Subscribe(created.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range backlog {
		if ev.Seq <= 3 {
			t.Fatalf("replay included seq %d after 3", ev.Seq)
		}
	}
}

func waitStatus(t *testing.T, m *Manager, id string, want Status) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if meta, ok, _ := m.Get(id); ok && meta.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	meta, _, _ := m.Get(id)
	t.Fatalf("status = %s, want %s", meta.Status, want)
}

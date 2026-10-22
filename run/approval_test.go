package run

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aixodia-engine/provider"
)

func TestApprovalGateWaitsForDecision(t *testing.T) {
	store := newMemoryStore()
	m := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}), store)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	created, _, err := m.Create(StartRequest{SessionID: "s1", ClientRunID: "c1", Message: "run a command"})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		allowed bool
		reason  string
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		allowed, reason, err := m.ApprovalGate(context.Background(), created.ID, provider.ToolCall{
			ID: "call_1", Name: "shell", Arguments: `{"command":"rm -rf /tmp/x"}`,
		})
		resultCh <- result{allowed, reason, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		meta, ok, _ := m.Get(created.ID)
		if ok && meta.Status == StatusWaitingApproval {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never entered waiting_approval: %+v", meta)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Approve(created.ID, "call_1", "deny", "not that file"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || got.allowed {
			t.Fatalf("decision = %+v", got)
		}
		if got.reason != "not that file" {
			t.Fatalf("reason = %q", got.reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate did not wake")
	}

	// The denial is in the log, and the status is back to running because the
	// model still gets to answer with the tool error.
	_, backlog, _, _, _, err := m.Subscribe(created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawWaiting, sawDenied bool
	for _, ev := range backlog {
		if ev.Kind != KindApproval {
			continue
		}
		var payload struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil {
			switch payload.Status {
			case "waiting":
				sawWaiting = true
			case "denied":
				sawDenied = true
			}
		}
	}
	if !sawWaiting || !sawDenied {
		t.Fatalf("approval log waiting=%v denied=%v events=%+v", sawWaiting, sawDenied, backlog)
	}
	m.Cancel(created.ID)
}

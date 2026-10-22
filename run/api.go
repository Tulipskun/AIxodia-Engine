package run

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handler serves the run API. Main mounts it behind the same bearer check as
// every other /v1 route.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", m.handleCreate)
	mux.HandleFunc("GET /v1/runs/{id}", m.handleGet)
	mux.HandleFunc("GET /v1/runs/{id}/events", m.handleEvents)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", m.handleCancel)
	mux.HandleFunc("POST /v1/runs/{id}/approve", m.handleApprove)
	return mux
}

func (m *Manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	created, existing, err := m.Create(req)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already running") {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	code := http.StatusAccepted
	if existing {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{"run": created})
}

func (m *Manager) handleGet(w http.ResponseWriter, r *http.Request) {
	meta, ok, err := m.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"message": "run not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": meta})
}

func (m *Manager) handleCancel(w http.ResponseWriter, r *http.Request) {
	meta, err := m.Cancel(r.PathValue("id"))
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": meta})
}

func (m *Manager) handleApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CallID   string `json:"call_id"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	meta, err := m.Approve(r.PathValue("id"), body.CallID, body.Decision, body.Reason)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not active") || strings.Contains(err.Error(), "not waiting") {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": meta})
}

func (m *Manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "after must be a non-negative integer"}})
			return
		}
		after = parsed
	} else if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > after {
			after = parsed
		}
	}

	meta, backlog, live, cancel, active, err := m.Subscribe(id, after)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	if meta.ID == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"message": "run not found"}})
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Run-Status", string(meta.Status))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeEvent := func(ev Event) bool {
		payload, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, payload); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	fmt.Fprint(w, "retry: 3000\n\n")
	SortEvents(backlog)
	for _, ev := range backlog {
		if !writeEvent(ev) {
			return
		}
	}
	if !active || live == nil || meta.Status.Terminal() {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case ev, ok := <-live:
			if !ok {
				return
			}
			if !writeEvent(ev) {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

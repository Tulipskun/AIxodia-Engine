package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"aixodia-engine/provider"
)

// Attachment is a file the phone uploaded before starting the run.
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

// StartRequest is one user turn. SessionID must already exist; the phone
// creates a draft with POST /v1/sessions as it does today.
type StartRequest struct {
	SessionID   string       `json:"session_id"`
	ClientRunID string       `json:"client_run_id,omitempty"`
	Message     string       `json:"message"`
	Retry       bool         `json:"retry,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Manager owns the runs this process is executing and serves their logs.
type Manager struct {
	store   Store
	handler http.Handler

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	runs map[string]*activeRun

	approvalMu    sync.RWMutex
	approvalTools map[string]bool
}

// New builds a manager over the gateway handler. The handler is called
// in-process with stream=true, so a run shares routing, history and
// persistence with POST /v1/chat/completions.
func New(handler http.Handler, store Store) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		store:   store,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		runs:    map[string]*activeRun{},
		approvalTools: map[string]bool{
			"shell": true,
			"write": true,
			"edit":  true,
		},
	}
}

// SetApprovalTools replaces the set of tool names that pause for a human
// decision. read/fetch_url/current_time are never in the default set.
func (m *Manager) SetApprovalTools(names []string) {
	next := map[string]bool{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			next[name] = true
		}
	}
	m.approvalMu.Lock()
	m.approvalTools = next
	m.approvalMu.Unlock()
}

func (m *Manager) needsApproval(name string) bool {
	m.approvalMu.RLock()
	defer m.approvalMu.RUnlock()
	return m.approvalTools[name]
}

// Start prepares the schema, marks runs left behind by an earlier process as
// interrupted, and starts the background persistence loop.
func (m *Manager) Start() error {
	if err := m.store.Ensure(); err != nil {
		return err
	}
	if err := m.store.InterruptStale(); err != nil {
		log.Printf("run: mark stale runs: %v", err)
	}
	go m.persistLoop()
	return nil
}

// Close stops accepting work and cancels the turns this process owns.
func (m *Manager) Close() { m.cancel() }

// Active is the number of runs that have not reached a terminal status. It is
// what the self-updater asks before replacing the binary.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.runs {
		r.mu.Lock()
		if !r.meta.Status.Terminal() {
			n++
		}
		r.mu.Unlock()
	}
	return n
}

// Create starts a run, or returns the existing one when client_run_id was
// already used. A retry that reached a lost response must not run the turn
// twice.
func (m *Manager) Create(req StartRequest) (Run, bool, error) {
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ClientRunID = strings.TrimSpace(req.ClientRunID)
	req.Message = strings.TrimSpace(req.Message)
	if req.SessionID == "" {
		return Run{}, false, fmt.Errorf("session_id is required")
	}
	if req.Message == "" && !req.Retry {
		return Run{}, false, fmt.Errorf("message must not be empty")
	}
	if req.ClientRunID != "" {
		if existing, ok, err := m.store.FindByClient(req.ClientRunID); err != nil {
			return Run{}, false, err
		} else if ok {
			return existing, true, nil
		}
	}

	m.mu.Lock()
	for _, r := range m.runs {
		r.mu.Lock()
		busy := r.meta.SessionID == req.SessionID && !r.meta.Status.Terminal()
		r.mu.Unlock()
		if busy {
			m.mu.Unlock()
			return Run{}, false, fmt.Errorf("a turn is already running for this chat")
		}
	}
	now := unixNow()
	meta := Run{
		ID:          newRunID(),
		SessionID:   req.SessionID,
		ClientRunID: req.ClientRunID,
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	ctx, cancel := context.WithCancel(m.ctx)
	a := &activeRun{meta: meta, cancel: cancel, store: m.store, subs: map[chan Event]struct{}{}, pending: map[string]*pendingApproval{}}
	m.runs[meta.ID] = a
	m.mu.Unlock()

	if err := m.store.Insert(meta); err != nil {
		m.mu.Lock()
		delete(m.runs, meta.ID)
		m.mu.Unlock()
		cancel()
		return Run{}, false, err
	}

	if req.Retry {
		a.append(KindUser, map[string]any{"retry": true})
	} else {
		a.append(KindUser, map[string]any{"text": req.Message})
	}
	a.append(KindStatus, map[string]any{"status": string(StatusRunning)})
	go m.execute(ctx, a, req)
	return a.snapshot(), false, nil
}

func (m *Manager) execute(ctx context.Context, a *activeRun, req StartRequest) {
	// Once the log is flushed, D1 is the source of truth. Keeping every finished
	// run in memory would make a long-lived engine grow without bound.
	defer m.forget(a.meta.ID)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("run: %s panicked: %v", a.meta.ID, recovered)
			a.finish(StatusFailed, fmt.Sprintf("internal error: %v", recovered))
		}
	}()

	payload := []map[string]string(nil)
	for _, attachment := range req.Attachments {
		payload = append(payload, map[string]string{
			"path": attachment.Path, "name": attachment.Name, "mime": attachment.Mime,
		})
	}
	var messages []map[string]string
	if !req.Retry {
		messages = []map[string]string{{"role": "user", "content": req.Message}}
	}
	body, err := json.Marshal(map[string]any{
		"session_id":  req.SessionID,
		"stream":      true,
		"append":      true,
		"retry":       req.Retry,
		"messages":    messages,
		"attachments": payload,
	})
	if err != nil {
		a.finish(StatusFailed, err.Error())
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		a.finish(StatusFailed, err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-Aixodia-Run", a.meta.ID)

	sink := newEventSink(a)
	m.handler.ServeHTTP(sink, httpReq)
	sink.close()

	switch {
	case ctx.Err() != nil:
		a.finish(StatusCancelled, "")
	case sink.errText != "":
		a.finish(StatusFailed, sink.errText)
	case sink.status != 0 && sink.status != http.StatusOK:
		text := strings.TrimSpace(sink.raw.String())
		if len(text) > 800 {
			text = text[:800] + "…"
		}
		if text == "" {
			text = fmt.Sprintf("chat returned HTTP %d", sink.status)
		}
		a.finish(StatusFailed, text)
	default:
		a.finish(StatusDone, "")
	}
}

// Cancel stops a live run. The worker records the terminal status once the
// chat handler returns, so callers can poll GET /v1/runs/{id}.
func (m *Manager) Cancel(id string) (Run, error) {
	a := m.lookup(id)
	if a == nil {
		meta, ok, err := m.store.Get(id)
		if err != nil {
			return Run{}, err
		}
		if !ok {
			return Run{}, fmt.Errorf("run not found")
		}
		return meta, fmt.Errorf("run is not active")
	}
	a.cancel()
	return a.snapshot(), nil
}

// ApprovalGate is installed on the gateway. Mutating tools pause here until
// POST /v1/runs/{id}/approve answers; read-only tools return immediately.
func (m *Manager) ApprovalGate(ctx context.Context, runID string, call provider.ToolCall) (bool, string, error) {
	if !m.needsApproval(call.Name) {
		return true, "", nil
	}
	a := m.lookup(runID)
	if a == nil {
		return false, "run is not active", nil
	}
	key := call.ID
	if key == "" {
		key = call.Name
	}
	pa := &pendingApproval{call: call, ch: make(chan approvalDecision, 1)}
	a.mu.Lock()
	a.pending[key] = pa
	a.meta.Status = StatusWaitingApproval
	a.meta.UpdatedAt = unixNow()
	a.mu.Unlock()

	a.append(KindStatus, map[string]any{"status": string(StatusWaitingApproval)})
	a.append(KindApproval, map[string]any{
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": truncateApproval(call.Arguments),
		"status":    "waiting",
	})
	a.persistMeta()

	select {
	case decision := <-pa.ch:
		a.mu.Lock()
		delete(a.pending, key)
		a.meta.Status = StatusRunning
		a.meta.UpdatedAt = unixNow()
		a.mu.Unlock()
		a.append(KindStatus, map[string]any{"status": string(StatusRunning)})
		status := "denied"
		if decision.allowed {
			status = "approved"
		}
		a.append(KindApproval, map[string]any{
			"call_id": call.ID,
			"name":    call.Name,
			"status":  status,
			"reason":  decision.reason,
		})
		a.persistMeta()
		return decision.allowed, decision.reason, nil
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		return false, "", ctx.Err()
	}
}

// Approve answers the pending tool call of a run. decision is "approve" or
// "deny"; an empty call_id means the only pending call.
func (m *Manager) Approve(runID, callID, decision, reason string) (Run, error) {
	a := m.lookup(runID)
	if a == nil {
		return Run{}, fmt.Errorf("run is not active")
	}
	allowed := false
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", "approved", "allow":
		allowed = true
	case "deny", "denied", "reject":
		allowed = false
	default:
		return Run{}, fmt.Errorf("decision must be approve or deny")
	}

	a.mu.Lock()
	if len(a.pending) == 0 {
		a.mu.Unlock()
		return Run{}, fmt.Errorf("this run is not waiting for approval")
	}
	key := callID
	pa, ok := a.pending[key]
	if !ok && key == "" {
		for k, only := range a.pending {
			key, pa, ok = k, only, true
			break
		}
	}
	if !ok {
		a.mu.Unlock()
		return Run{}, fmt.Errorf("no pending approval for call %q", callID)
	}
	// Remove it before waking the gate so a second tap cannot answer twice.
	delete(a.pending, key)
	meta := a.meta
	a.mu.Unlock()

	pa.ch <- approvalDecision{allowed: allowed, reason: reason}
	return meta, nil
}

func (a *activeRun) persistMeta() {
	a.mu.Lock()
	meta := a.meta
	a.mu.Unlock()
	if err := a.store.Update(meta); err != nil {
		log.Printf("run: %s: save status: %v", meta.ID, err)
	}
}

func truncateApproval(text string) string {
	const limit = 8 * 1024
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.runs, id)
	m.mu.Unlock()
}

func (m *Manager) lookup(id string) *activeRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// Get returns run metadata from memory, or from D1 after a restart.
func (m *Manager) Get(id string) (Run, bool, error) {
	if a := m.lookup(id); a != nil {
		return a.snapshot(), true, nil
	}
	return m.store.Get(id)
}

// Subscribe returns the backlog after seq and, while the run is live, a
// channel of new events. The channel is closed when the run finishes or the
// viewer falls too far behind; a closed channel with new events in the store
// means "reconnect with the last seq you saw".
func (m *Manager) Subscribe(id string, after int64) (Run, []Event, <-chan Event, func(), bool, error) {
	if a := m.lookup(id); a != nil {
		meta, backlog, ch, cancel := a.subscribe(after)
		return meta, backlog, ch, cancel, true, nil
	}
	meta, ok, err := m.store.Get(id)
	if err != nil {
		return Run{}, nil, nil, nil, false, err
	}
	if !ok {
		return Run{}, nil, nil, nil, false, nil
	}
	events, err := m.store.Events(id, after)
	if err != nil {
		return Run{}, nil, nil, nil, false, err
	}
	return meta, events, nil, func() {}, false, nil
}

func (m *Manager) persistLoop() {
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			runs := make([]*activeRun, 0, len(m.runs))
			for _, a := range m.runs {
				runs = append(runs, a)
			}
			m.mu.Unlock()
			for _, a := range runs {
				a.flush(m.store)
			}
		}
	}
}

// ---- activeRun ----

type activeRun struct {
	mu sync.Mutex

	meta   Run
	events []Event
	subs   map[chan Event]struct{}

	persisted int
	flushMu   sync.Mutex
	store     Store

	pending map[string]*pendingApproval

	cancel context.CancelFunc
}

type pendingApproval struct {
	call provider.ToolCall
	ch   chan approvalDecision
}

type approvalDecision struct {
	allowed bool
	reason  string
}

func (a *activeRun) snapshot() Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meta
}

func (a *activeRun) append(kind string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.meta.LastSeq++
	ev := Event{runID: a.meta.ID, Seq: a.meta.LastSeq, Kind: kind, At: unixNow(), Data: raw}
	a.meta.UpdatedAt = ev.At
	a.events = append(a.events, ev)
	for ch := range a.subs {
		select {
		case ch <- ev:
		default:
			// A viewer that cannot keep up must reconnect from its last seq;
			// blocking here would stall the agent turn itself.
			delete(a.subs, ch)
			close(ch)
		}
	}
	a.mu.Unlock()
}

func (a *activeRun) finish(status Status, errText string) {
	a.mu.Lock()
	a.meta.Status = status
	a.meta.Error = errText
	a.meta.UpdatedAt = unixNow()
	a.meta.LastSeq++
	ev := Event{
		runID: a.meta.ID,
		Seq:   a.meta.LastSeq,
		Kind:  KindStatus,
		At:    a.meta.UpdatedAt,
		Data:  mustJSON(map[string]any{"status": string(status), "error": errText}),
	}
	a.events = append(a.events, ev)
	for ch := range a.subs {
		select {
		case ch <- ev:
		default:
		}
		close(ch)
	}
	a.subs = map[chan Event]struct{}{}
	meta := a.meta
	a.mu.Unlock()

	a.flush(a.store)
	if err := a.store.Update(meta); err != nil {
		log.Printf("run: %s: save status: %v", meta.ID, err)
	}
}

func (a *activeRun) subscribe(after int64) (Run, []Event, chan Event, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	meta := a.meta
	var backlog []Event
	for _, ev := range a.events {
		if ev.Seq > after {
			backlog = append(backlog, ev)
		}
	}
	if meta.Status.Terminal() {
		return meta, backlog, nil, func() {}
	}
	ch := make(chan Event, 256)
	a.subs[ch] = struct{}{}
	cancel := func() {
		a.mu.Lock()
		if _, ok := a.subs[ch]; ok {
			delete(a.subs, ch)
			close(ch)
		}
		a.mu.Unlock()
	}
	return meta, backlog, ch, cancel
}

func (a *activeRun) flush(store Store) {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	a.mu.Lock()
	start := a.persisted
	if start >= len(a.events) {
		a.mu.Unlock()
		return
	}
	batch := append([]Event(nil), a.events[start:]...)
	a.mu.Unlock()

	if err := store.AppendEvents(batch); err != nil {
		log.Printf("run: %s: save %d events: %v", a.meta.ID, len(batch), err)
		return
	}
	a.mu.Lock()
	if a.persisted == start {
		a.persisted = start + len(batch)
	}
	a.mu.Unlock()
}

func newRunID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return "run_" + hex.EncodeToString(buf[:])
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

// ---- SSE sink ----

type eventSink struct {
	a *activeRun

	header  http.Header
	status  int
	pending bytes.Buffer
	raw     bytes.Buffer

	mu          sync.Mutex
	assistant   strings.Builder
	reasoning   strings.Builder
	reasonRound int
	lastFlush   time.Time
	timer       *time.Timer
	closed      bool
	errText     string
}

func newEventSink(a *activeRun) *eventSink {
	return &eventSink{a: a, header: http.Header{}, lastFlush: time.Now()}
}

func (s *eventSink) Header() http.Header { return s.header }
func (s *eventSink) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
}

// Flush is intentionally not the gateway's flush: assistant text is coalesced
// on a short timer so a fast model does not write hundreds of database rows a
// second.
func (s *eventSink) Flush() {}

func (s *eventSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	// raw is only used to describe a non-200 chat response. A long successful
	// turn must not pin its whole transcript here.
	if s.raw.Len() < 8<<10 {
		remaining := 8<<10 - s.raw.Len()
		if len(p) > remaining {
			s.raw.Write(p[:remaining])
		} else {
			s.raw.Write(p)
		}
	}
	s.pending.Write(p)
	for {
		buf := s.pending.Bytes()
		idx := bytes.Index(buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := append([]byte(nil), buf[:idx]...)
		s.pending.Next(idx + 2)
		if bytes.HasPrefix(event, []byte("data: ")) {
			s.handleLocked(event[6:])
		}
	}
	s.mu.Unlock()
	return len(p), nil
}

func (s *eventSink) handleLocked(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		s.flushLocked()
		return
	}
	var ev struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Turns []json.RawMessage `json:"turns"`
		Turn  json.RawMessage   `json:"turn"`
		Usage json.RawMessage   `json:"usage"`
		Tool  *struct {
			Stage string `json:"stage"`
			Round int    `json:"round"`
			Text  string `json:"text"`
		} `json:"tool_event"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	if ev.Error != nil {
		s.flushLocked()
		s.errText = ev.Error.Message
		s.a.append(KindError, map[string]any{"message": ev.Error.Message})
		return
	}
	if ev.Tool != nil {
		if ev.Tool.Stage == "reasoning" && ev.Tool.Text != "" {
			s.reasoning.WriteString(ev.Tool.Text)
			s.reasonRound = ev.Tool.Round
			s.scheduleLocked()
			return
		}
		s.flushLocked()
		// Keep the original payload: usage figures and the presentation block
		// are fields this sink does not need to understand, but the phone does.
		var wrapped map[string]json.RawMessage
		if json.Unmarshal(data, &wrapped) == nil {
			if raw, ok := wrapped["tool_event"]; ok {
				s.a.append(KindTool, raw)
			}
		}
		return
	}
	for _, turn := range ev.Turns {
		s.flushLocked()
		s.a.append(KindTurn, turn)
	}
	if len(ev.Turn) > 0 {
		s.flushLocked()
		s.a.append(KindFinal, map[string]any{"turn": ev.Turn, "usage": ev.Usage})
		return
	}
	for _, choice := range ev.Choices {
		if choice.Delta.Content == "" {
			continue
		}
		s.assistant.WriteString(choice.Delta.Content)
		s.scheduleLocked()
	}
}

func (s *eventSink) scheduleLocked() {
	if s.closed {
		return
	}
	wait := 120*time.Millisecond - time.Since(s.lastFlush)
	if wait <= 0 {
		s.flushLocked()
		return
	}
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(wait, func() {
		s.mu.Lock()
		s.timer = nil
		s.flushLocked()
		s.mu.Unlock()
	})
}

func (s *eventSink) flushLocked() {
	s.lastFlush = time.Now()
	if text := s.assistant.String(); text != "" {
		s.a.append(KindAssistant, map[string]any{"text": text})
		s.assistant.Reset()
	}
	if text := s.reasoning.String(); text != "" {
		s.a.append(KindReasoning, map[string]any{"text": text, "round": s.reasonRound})
		s.reasoning.Reset()
	}
}

func (s *eventSink) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.flushLocked()
	s.mu.Unlock()
}

// SortEvents is used by the API before encoding a static replay, so a store
// that returned rows out of order cannot make the phone apply them twice.
func SortEvents(events []Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
}

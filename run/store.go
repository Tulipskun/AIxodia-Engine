package run

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"aixodia-engine/db"
)

// Store is everything the manager needs to survive its own process. The D1
// implementation is the real one; memoryStore is used by tests.
type Store interface {
	Ensure() error
	Insert(Run) error
	Update(Run) error
	Get(id string) (Run, bool, error)
	FindByClient(clientID string) (Run, bool, error)
	InterruptStale() error
	AppendEvents([]Event) error
	Events(runID string, after int64) ([]Event, error)
}

// D1Store keeps runs and their events in Cloudflare D1, beside the chat history
// the runs write. Events are inserted in small batches because D1 limits the
// number of bound parameters in one statement.
type D1Store struct{}

func NewD1Store() *D1Store { return &D1Store{} }

func (D1Store) Ensure() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			client_run_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			last_seq INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS runs_client_run_id
			ON runs(client_run_id) WHERE client_run_id != ''`,
		`CREATE INDEX IF NOT EXISTS runs_session_id ON runs(session_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS run_events (
			run_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			kind TEXT NOT NULL,
			data TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (run_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS run_events_run_seq ON run_events(run_id, seq)`,
	}
	for _, statement := range statements {
		if _, err := db.Query(statement); err != nil {
			return fmt.Errorf("run: schema: %w", err)
		}
	}
	return nil
}

func (D1Store) Insert(r Run) error {
	_, err := db.Query(
		`INSERT INTO runs (id,session_id,client_run_id,status,error,created_at,updated_at,last_seq)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.SessionID, r.ClientRunID, string(r.Status), r.Error, r.CreatedAt, r.UpdatedAt, r.LastSeq)
	return err
}

func (D1Store) Update(r Run) error {
	_, err := db.Query(
		`UPDATE runs SET status=?, error=?, updated_at=?, last_seq=? WHERE id=?`,
		string(r.Status), r.Error, r.UpdatedAt, r.LastSeq, r.ID)
	return err
}

func (D1Store) Get(id string) (Run, bool, error) {
	rows, err := db.Query(`SELECT id,session_id,client_run_id,status,error,created_at,updated_at,last_seq FROM runs WHERE id=? LIMIT 1`, id)
	if err != nil || len(rows) == 0 {
		return Run{}, false, err
	}
	return runFromRow(rows[0]), true, nil
}

func (D1Store) FindByClient(clientID string) (Run, bool, error) {
	if strings.TrimSpace(clientID) == "" {
		return Run{}, false, nil
	}
	rows, err := db.Query(`SELECT id,session_id,client_run_id,status,error,created_at,updated_at,last_seq FROM runs WHERE client_run_id=? LIMIT 1`, clientID)
	if err != nil || len(rows) == 0 {
		return Run{}, false, err
	}
	return runFromRow(rows[0]), true, nil
}

// InterruptStale marks work this process cannot still be running. A goroutine
// does not survive a restart; saying "running" would make the phone wait for
// events that will never arrive.
func (D1Store) InterruptStale() error {
	_, err := db.Query(
		`UPDATE runs SET status='interrupted', updated_at=unixepoch(), error=CASE WHEN error='' THEN 'engine restarted' ELSE error END
		 WHERE status IN ('running','waiting_approval')`)
	return err
}

func (D1Store) AppendEvents(events []Event) error {
	for start := 0; start < len(events); start += 16 {
		end := start + 16
		if end > len(events) {
			end = len(events)
		}
		batch := events[start:end]
		placeholders := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*5)
		for _, ev := range batch {
			placeholders = append(placeholders, "(?,?,?,?,?)")
			args = append(args, ev.runID, ev.Seq, ev.Kind, string(ev.Data), ev.At)
		}
		if _, err := db.Query(
			`INSERT INTO run_events (run_id,seq,kind,data,created_at) VALUES `+
				strings.Join(placeholders, ",")+
				` ON CONFLICT(run_id,seq) DO NOTHING`, args...); err != nil {
			return err
		}
	}
	return nil
}

func (D1Store) Events(runID string, after int64) ([]Event, error) {
	rows, err := db.Query(
		`SELECT seq,kind,data,created_at FROM run_events WHERE run_id=? AND seq>? ORDER BY seq ASC`,
		runID, after)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, Event{
			Seq:  int64Field(row, "seq"),
			Kind: stringField(row, "kind"),
			At:   int64Field(row, "created_at"),
			Data: []byte(stringField(row, "data")),
		})
	}
	return out, nil
}

func runFromRow(row map[string]any) Run {
	return Run{
		ID:          stringField(row, "id"),
		SessionID:   stringField(row, "session_id"),
		ClientRunID: stringField(row, "client_run_id"),
		Status:      Status(stringField(row, "status")),
		Error:       stringField(row, "error"),
		CreatedAt:   int64Field(row, "created_at"),
		UpdatedAt:   int64Field(row, "updated_at"),
		LastSeq:     int64Field(row, "last_seq"),
	}
}

func stringField(row map[string]any, key string) string {
	if value, ok := row[key].(string); ok {
		return value
	}
	return fmt.Sprint(row[key])
}

func int64Field(row map[string]any, key string) int64 {
	switch value := row[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case string:
		var parsed int64
		_, _ = fmt.Sscan(value, &parsed)
		return parsed
	}
	return 0
}

// memoryStore is the test store. It is deliberately simple and keeps ordering
// explicit so replay tests can assert exact seq values.
type memoryStore struct {
	mu     sync.Mutex
	runs   map[string]Run
	events map[string][]Event
}

func newMemoryStore() *memoryStore {
	return &memoryStore{runs: map[string]Run{}, events: map[string][]Event{}}
}

func (m *memoryStore) Ensure() error { return nil }

func (m *memoryStore) Insert(r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ClientRunID != "" {
		for _, existing := range m.runs {
			if existing.ClientRunID == r.ClientRunID {
				return fmt.Errorf("duplicate client run id")
			}
		}
	}
	m.runs[r.ID] = r
	return nil
}

func (m *memoryStore) Update(r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r
	return nil
}

func (m *memoryStore) Get(id string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok, nil
}

func (m *memoryStore) FindByClient(clientID string) (Run, bool, error) {
	if clientID == "" {
		return Run{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runs {
		if r.ClientRunID == clientID {
			return r, true, nil
		}
	}
	return Run{}, false, nil
}

func (m *memoryStore) InterruptStale() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runs {
		if !r.Status.Terminal() {
			r.Status = StatusInterrupted
			r.Error = "engine restarted"
			r.UpdatedAt = unixNow()
			m.runs[id] = r
		}
	}
	return nil
}

func (m *memoryStore) AppendEvents(events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range events {
		list := m.events[ev.runID]
		list = append(list, ev)
		sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
		m.events[ev.runID] = list
	}
	return nil
}

func (m *memoryStore) Events(runID string, after int64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, ev := range m.events[runID] {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, nil
}

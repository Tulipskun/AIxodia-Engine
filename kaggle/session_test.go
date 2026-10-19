package kaggle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recorder stands in for kgat_sync.py: it remembers the actions in order and
// can fail a chosen number of times.
type recorder struct {
	mu    sync.Mutex
	acts  []string
	fail  map[string]int
	calls int
}

func (r *recorder) exec(_ context.Context, _ string, action string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.acts = append(r.acts, action)
	if r.fail[action] > 0 {
		r.fail[action]--
		return errors.New("boom")
	}
	return nil
}

func (r *recorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.acts...)
}

func TestExitFileSnapshotsThenRestarts(t *testing.T) {
	dir := t.TempDir()
	exit := filepath.Join(dir, ".exit")
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	cfg := Config{
		Script:   "/usr/local/bin/kgat_sync.py",
		ExitFile: exit,
		Every:    5 * time.Millisecond,
		Limit:    time.Hour,
		Exec:     rec.exec,
	}
	res, err := cfg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != "exit file" || !res.BackedUp {
		t.Fatalf("result = %+v, want reason=exit file backedUp=true", res)
	}
	if got := rec.actions(); len(got) != 2 || got[0] != "backup" || got[1] != "restart" {
		t.Fatalf("actions = %v, want [backup restart]", got)
	}
	if _, err := os.Stat(exit); !os.IsNotExist(err) {
		t.Fatalf("exit file was not removed (%v) — the next session would exit at once", err)
	}
}

func TestSessionLimitTriggersWithoutAnExitFile(t *testing.T) {
	rec := &recorder{}
	cfg := Config{
		Script:   "/usr/local/bin/kgat_sync.py",
		ExitFile: filepath.Join(t.TempDir(), "missing"),
		Every:    2 * time.Millisecond,
		Limit:    15 * time.Millisecond,
		Exec:     rec.exec,
	}
	res, err := cfg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != "session limit" {
		t.Fatalf("reason = %q, want session limit", res.Reason)
	}
	if got := rec.actions(); len(got) != 2 || got[1] != "restart" {
		t.Fatalf("actions = %v, want backup then restart", got)
	}
}

func TestFailedBackupIsRetriedAndNotRestarted(t *testing.T) {
	dir := t.TempDir()
	exit := filepath.Join(dir, ".exit")
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{fail: map[string]int{"backup": 1}}
	cfg := Config{
		Script:   "/usr/local/bin/kgat_sync.py",
		ExitFile: exit,
		Every:    2 * time.Millisecond,
		Retry:    5 * time.Millisecond,
		Grace:    time.Minute,
		Limit:    time.Hour,
		Exec:     rec.exec,
	}
	res, err := cfg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.BackedUp {
		t.Fatalf("result = %+v, want backedUp", res)
	}
	if got := rec.actions(); len(got) != 3 || got[0] != "backup" || got[1] != "backup" || got[2] != "restart" {
		t.Fatalf("actions = %v, want backup, backup (retry), restart", got)
	}
}

func TestGivesUpWhenRetryCannotFitBeforeTheHardDeadline(t *testing.T) {
	dir := t.TempDir()
	exit := filepath.Join(dir, ".exit")
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{fail: map[string]int{"backup": 99}}
	cfg := Config{
		Script:   "/usr/local/bin/kgat_sync.py",
		ExitFile: exit,
		Every:    2 * time.Millisecond,
		Retry:    time.Hour,
		Grace:    time.Minute,
		Limit:    time.Minute,
		Exec:     rec.exec,
	}
	if _, err := cfg.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded although the backup kept failing")
	}
	if got := rec.actions(); len(got) != 1 {
		t.Fatalf("actions = %v, want a single backup attempt", got)
	}
}

func TestContextCancelBeforeTheSessionIsOver(t *testing.T) {
	rec := &recorder{}
	cfg := Config{
		Script:   "/usr/local/bin/kgat_sync.py",
		ExitFile: filepath.Join(t.TempDir(), "missing"),
		Every:    2 * time.Millisecond,
		Limit:    time.Hour,
		Exec:     rec.exec,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := cfg.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if res.Reason != "" {
		t.Fatalf("reason = %q, want empty", res.Reason)
	}
	if got := rec.actions(); len(got) != 0 {
		t.Fatalf("actions = %v, want none", got)
	}
}

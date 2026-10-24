package selfupdate

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The signal is what `aixodia update` sends instead of stopping the daemon: it
// must reach Apply with the published release, and Apply is the call that execs
// the process in place. Swapping apply keeps the test from execing itself.
func TestUpdateSignalAppliesThePublishedBuild(t *testing.T) {
	savedNeed, savedApply := need, apply
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		need, apply = savedNeed, savedApply
	}()

	rel := Release{URL: "http://example.invalid/engine", UpdatedAt: "2026-10-21T00:00:00Z"}
	mu.Lock()
	need = func(context.Context) (bool, Release, error) { return true, rel, nil }
	mu.Unlock()

	applied := make(chan string, 1)
	mu.Lock()
	apply = func(_ context.Context, got Release, target string) error {
		applied <- got.UpdatedAt
		return nil
	}
	mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NotifyUpdate(ctx, func() bool { return true })

	if err := syscall.Kill(syscall.Getpid(), UpdateSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-applied:
		if got != rel.UpdatedAt {
			t.Errorf("applied release = %q, want %q", got, rel.UpdatedAt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the update signal did not reach Apply")
	}
}

// A chat in flight must not be cut short by a deploy: the request is noted and
// left for the next tick, so nothing is installed while the turn runs.
func TestUpdateSignalWaitsForAnIdleEngine(t *testing.T) {
	savedNeed, savedApply := need, apply
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		need, apply = savedNeed, savedApply
	}()

	mu.Lock()
	need = func(context.Context) (bool, Release, error) {
		return true, Release{UpdatedAt: "2026-10-21T00:00:00Z"}, nil
	}
	mu.Unlock()
	applied := make(chan struct{}, 1)
	mu.Lock()
	apply = func(context.Context, Release, string) error {
		applied <- struct{}{}
		return nil
	}
	mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NotifyUpdate(ctx, func() bool { return false })

	if err := syscall.Kill(syscall.Getpid(), UpdateSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
		t.Fatal("a busy engine must not apply an update")
	case <-time.After(300 * time.Millisecond):
	}
}

// Nothing published means nothing to do: the signal must not fall through to
// Apply, which would swap a binary for the one already running.
func TestUpdateSignalDoesNothingWhenTheBuildMatches(t *testing.T) {
	savedNeed, savedApply := need, apply
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		need, apply = savedNeed, savedApply
	}()

	called := false
	mu.Lock()
	need = func(context.Context) (bool, Release, error) { return false, Release{}, nil }
	mu.Unlock()
	mu.Lock()
	apply = func(context.Context, Release, string) error {
		called = true
		return nil
	}
	mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NotifyUpdate(ctx, func() bool { return true })

	if err := syscall.Kill(syscall.Getpid(), UpdateSignal); err != nil {
		t.Fatal(err)
	}
	// The handler runs asynchronously; give it a chance to (wrongly) apply.
	time.Sleep(300 * time.Millisecond)
	if called {
		t.Error("Apply ran for a build that was already in place")
	}
}

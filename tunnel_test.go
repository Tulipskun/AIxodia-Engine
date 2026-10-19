package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCloudflared writes an executable stand-in for cloudflared. It logs to
// stderr, the stream cloudflared itself logs to, so the tests drive the real
// process plumbing without Cloudflare.
func fakeCloudflared(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitURL(t *testing.T, urls <-chan string) string {
	t.Helper()
	select {
	case u := <-urls:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no tunnel url was published in time")
		return ""
	}
}

// A URL is announced before the tunnel is reachable; only the registration line
// means the hostname actually resolves. Publishing the earlier one is the bug
// that left D1 naming a URL no phone could dial.
func TestStartCloudflaredPublishesOnlyOnceRegistered(t *testing.T) {
	path := fakeCloudflared(t, `echo 'INF Your quick Tunnel has been created! Visit it at https://live.trycloudflare.com' >&2
echo 'INF Registered tunnel connection connIndex=0' >&2
sleep 30
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tn, err := startCloudflared(ctx, path)
	if err != nil {
		t.Fatalf("startCloudflared: %v", err)
	}
	if got := tn.URL(); got != "https://live.trycloudflare.com" {
		t.Fatalf("url = %q", got)
	}
	if !tn.Alive() {
		t.Fatal("a running cloudflared must read as alive")
	}
}

func TestStartCloudflaredFailsWhenTheTunnelNeverRegisters(t *testing.T) {
	path := fakeCloudflared(t, `echo 'INF Your quick Tunnel has been created! Visit it at https://dead.trycloudflare.com' >&2
echo 'ERR no more connections active and exiting' >&2
exit 1
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := startCloudflared(ctx, path); err == nil {
		t.Fatal("a tunnel that never registered must not yield a URL")
	}
}

func TestTunnelIsNotAliveAfterCloudflaredExits(t *testing.T) {
	path := fakeCloudflared(t, `echo 'INF Your quick Tunnel has been created! Visit it at https://gone.trycloudflare.com' >&2
echo 'INF Registered tunnel connection connIndex=0' >&2
sleep 1
exit 1
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tn, err := startCloudflared(ctx, path)
	if err != nil {
		t.Fatalf("startCloudflared: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tn.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tn.Alive() {
		t.Fatal("cloudflared has exited; the tunnel must read as not alive")
	}
}

// The daemon row names the URL of the live tunnel: when cloudflared exits and a
// new one comes up with a different address, that address must be republished.
func TestKeeperRepublishesTheURLWhenCloudflaredRestarts(t *testing.T) {
	path := fakeCloudflared(t, `n=$(cat "$0.n" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "$0.n"
echo "INF Your quick Tunnel has been created! Visit it at https://t$n.trycloudflare.com" >&2
echo 'INF Registered tunnel connection connIndex=0' >&2
if [ "$n" -ge 2 ]; then sleep 30; else sleep 1; fi
`)
	urls := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k := newKeeper(path, func(u string) { urls <- u })
	go k.run(ctx)

	if got := waitURL(t, urls); got != "https://t1.trycloudflare.com" {
		t.Fatalf("first url = %q", got)
	}
	if got := waitURL(t, urls); got != "https://t2.trycloudflare.com" {
		t.Fatalf("second url = %q", got)
	}
}

// The app reads a fresh daemon/heartbeat as "online". While the tunnel is down
// the heartbeat must not be refreshed, or the phone adopts a URL that is dead.
func TestHeartbeatSkipsTheWriteWhileTheTunnelIsDown(t *testing.T) {
	var touched int
	deps := heartbeatDeps{
		newer: func(int64) (bool, error) { return false, nil },
		touch: func(string, int64) error { touched++; return nil },
		now:   func() int64 { return 1 },
	}
	if !heartbeatOnce(100, func() bool { return false }, deps) {
		t.Fatal("a dead tunnel must not stop the engine")
	}
	if touched != 0 {
		t.Fatalf("heartbeat refreshed %d time(s) with a dead tunnel", touched)
	}
}

func TestHeartbeatRefreshesWhileTheTunnelIsUp(t *testing.T) {
	var touched int
	deps := heartbeatDeps{
		newer: func(int64) (bool, error) { return false, nil },
		touch: func(string, int64) error { touched++; return nil },
		now:   func() int64 { return 1 },
	}
	if !heartbeatOnce(100, func() bool { return true }, deps) {
		t.Fatal("a live tunnel keeps the heartbeat running")
	}
	if touched != 1 {
		t.Fatalf("heartbeat refreshed %d time(s), want 1", touched)
	}
}

func TestHeartbeatStopsWhenANewerEngineTookOver(t *testing.T) {
	deps := heartbeatDeps{
		newer: func(int64) (bool, error) { return true, nil },
		touch: func(string, int64) error { return nil },
		now:   func() int64 { return 1 },
	}
	if heartbeatOnce(100, func() bool { return true }, deps) {
		t.Fatal("a newer engine must stop this one")
	}
}

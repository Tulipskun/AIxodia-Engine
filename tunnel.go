package main

import (
	"bufio"
	"context"
	"errors"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// tunnelURL is the public address cloudflared announces for a quick tunnel.
var tunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

// registeredLine is what cloudflared prints once it has actually joined the
// edge. The URL is announced earlier and only starts resolving after this line,
// so this is the moment a URL is safe to publish.
const registeredLine = "Registered tunnel connection"

// tunnel is one running cloudflared quick tunnel.
type tunnel struct {
	url  string
	done chan struct{}
}

func (t *tunnel) URL() string { return t.url }

// Alive reports whether cloudflared is still running. It is the daemon's real
// health: the HTTP server can keep serving while the tunnel that makes it
// reachable is already gone.
func (t *tunnel) Alive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// startCloudflared runs cloudflared and waits until the quick tunnel has
// registered with the edge, so the URL it returns is one that actually
// resolves. It needs the cloudflared binary on the given path.
func startCloudflared(ctx context.Context, path string) (*tunnel, error) {
	cmd := exec.Command(path, "tunnel", "--no-autoupdate",
		"--metrics", "127.0.0.1:0", "--url", "http://127.0.0.1:8787")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	t := &tunnel{done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(t.done)
	}()

	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		url := ""
		for scanner.Scan() {
			line := scanner.Text()
			if m := tunnelURL.FindString(line); m != "" {
				url = m
			}
			if url != "" && strings.Contains(line, registeredLine) {
				select {
				case ready <- url:
				default:
				}
			}
		}
	}()

	select {
	case url := <-ready:
		t.url = url
		go func() {
			<-ctx.Done()
			_ = cmd.Process.Kill()
		}()
		return t, nil
	case <-t.done:
		return nil, errors.New("cloudflared exited before the tunnel registered")
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		return nil, errors.New("timed out waiting for the trycloudflare URL")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}
}

// keeper owns the cloudflared quick tunnel. It starts one, hands every
// (re)start's URL to publish, and starts a fresh one when it exits: the daemon
// row must never keep naming a URL whose tunnel has died.
type keeper struct {
	path    string
	publish func(url string)

	mu  sync.Mutex
	cur *tunnel
}

func newKeeper(path string, publish func(string)) *keeper {
	return &keeper{path: path, publish: publish}
}

// alive reports whether the tunnel is up right now. The heartbeat asks before
// refreshing the daemon keys: a dead tunnel is not a live daemon.
func (k *keeper) alive() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cur != nil && k.cur.Alive()
}

// URL is the tunnel this keeper currently has up, empty while it has none.
func (k *keeper) URL() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cur == nil {
		return ""
	}
	return k.cur.URL()
}

func (k *keeper) set(t *tunnel) {
	k.mu.Lock()
	k.cur = t
	k.mu.Unlock()
}

// run keeps a tunnel up for the life of ctx.
func (k *keeper) run(ctx context.Context) {
	for ctx.Err() == nil {
		t, err := startCloudflared(ctx, k.path)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tunnel: %v", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		k.set(t)
		log.Printf("tunnel url: %s", t.URL())
		if k.publish != nil {
			k.publish(t.URL())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.done:
		}
		k.set(nil)
		log.Print("tunnel: cloudflared exited; restarting")
	}
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

package terminal

import (
	"strings"
	"testing"
	"time"
)

// readUntil pumps the pty into a buffer until marker appears, then returns
// everything it read. Reading a pty blocks until the shell writes or the pty
// goes away, so the pump runs in its own goroutine and the caller bounds the
// wait; the pump stops when the test stops caring.
func readUntil(t *testing.T, p *PTY, marker string, within time.Duration) string {
	t.Helper()
	type chunk struct {
		data []byte
		err  error
	}
	done := make(chan struct{})
	defer close(done)
	ch := make(chan chunk, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				select {
				case ch <- chunk{data: append([]byte(nil), buf[:n]...)}:
				case <-done:
					return
				}
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()

	var out strings.Builder
	deadline := time.After(within)
	for {
		if strings.Contains(out.String(), marker) {
			return out.String()
		}
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("readUntil(%q): pty closed after %q", marker, out.String())
			}
			out.Write(c.data)
		case <-deadline:
			t.Fatalf("readUntil(%q): timed out after %q", marker, out.String())
		}
	}
}

// waitWithin bounds a Wait: a pty that never reaps its shell must fail the test
// rather than hang it.
func waitWithin(t *testing.T, p *PTY, within time.Duration) int {
	t.Helper()
	ch := make(chan int, 1)
	go func() { ch <- p.Wait() }()
	select {
	case code := <-ch:
		return code
	case <-time.After(within):
		t.Fatalf("Wait did not return within %s", within)
		return 0
	}
}

func TestPTYShellSeesWindowSize(t *testing.T) {
	p, err := StartPTY("/bin/sh", 100, 30, t.TempDir())
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	defer p.Close()
	if _, err := p.Write([]byte("stty size\nexit 7\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The command echoes as it is typed, so wait for its output, which cannot
	// come from the echo.
	readUntil(t, p, "30 100", 5*time.Second)
	if code := waitWithin(t, p, 5*time.Second); code != 7 {
		t.Fatalf("Wait() = %d, want 7", code)
	}
}

func TestPTYResizeSignalsTheShell(t *testing.T) {
	p, err := StartPTY("/bin/sh", 80, 24, t.TempDir())
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	defer p.Close()
	if err := p.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := p.Write([]byte("stty size\nexit 0\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, p, "40 120", 5*time.Second)
	if code := waitWithin(t, p, 5*time.Second); code != 0 {
		t.Fatalf("Wait() = %d, want 0", code)
	}
}

func TestPTYShellDeathIsASignal(t *testing.T) {
	p, err := StartPTY("/bin/sh", 80, 24, "")
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	defer p.Close()
	// SIGKILL because an interactive dash ignores TERM and INT aimed at the
	// shell itself (verified on this host), and the point here is the exit
	// code of a shell the kernel killed rather than the shell's own exit.
	if _, err := p.Write([]byte("kill -KILL $$\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := waitWithin(t, p, 5*time.Second); code != -1 {
		t.Fatalf("Wait() = %d, want -1 for a shell killed by a signal", code)
	}
}

func TestPTYCloseKillsTheShell(t *testing.T) {
	p, err := StartPTY("/bin/sh", 80, 24, "")
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	if _, err := p.Write([]byte("exec sleep 300\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	code := waitWithin(t, p, 5*time.Second)
	if code == 0 {
		t.Fatalf("Wait() = 0, want the signalled sleep to leave a non-zero code")
	}
}

func TestPTYMissingShellFails(t *testing.T) {
	p, err := StartPTY("/nonexistent/sh", 80, 24, "")
	if err == nil {
		p.Close()
		t.Fatalf("StartPTY accepted a shell that does not exist")
	}
	if p != nil {
		t.Fatalf("StartPTY returned a pty alongside %v", err)
	}
}

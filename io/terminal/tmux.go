package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// tmuxSocket is a private server so AIxodia's sessions never appear in, or
// collide with, the user's own tmux.
const tmuxSocket = "aixodia"

func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func tmuxName(id string) string {
	var b strings.Builder
	b.WriteString("aix-")
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// killTmux ends a shell for good. Closing the pty only detaches the client;
// the tmux server would keep the shell alive and the phone would reattach to a
// tab it asked to close.
func killTmux(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "tmux", "-L", tmuxSocket, "kill-session", "-t", tmuxName(id)).Run()
}

// listTmux returns sessions that survived an engine restart. The in-memory map
// is empty after a restart, so without this the phone would think every saved
// tab is gone and start a new shell under a new id.
func listTmux() []Info {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	format := "#{session_name}\x1f#{pane_pid}\x1f#{pane_current_path}\x1f#{session_created}"
	raw, err := exec.CommandContext(ctx, "tmux", "-L", tmuxSocket, "list-sessions", "-F", format).Output()
	if err != nil {
		return nil
	}
	var out []Info
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.SplitN(line, "\x1f", 4)
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "aix-") {
			continue
		}
		id := strings.TrimPrefix(fields[0], "aix-")
		pid, _ := strconv.Atoi(fields[1])
		created, _ := strconv.ParseInt(fields[3], 10, 64)
		out = append(out, Info{
			ID:        id,
			PID:       pid,
			Shell:     "tmux",
			Cwd:       fields[2],
			Alive:     true,
			CreatedAt: created,
		})
	}
	return out
}

// tmuxHasSession reports whether the private server already holds name.
func tmuxHasSession(name string) bool {
	return exec.Command("tmux", "-L", tmuxSocket, "has-session", "-t", name).Run() == nil
}

// startTmux opens the pty that carries a tmux client. new-session -A attaches
// to the named session when it already exists, which is exactly the reattach
// after the engine replaced itself.
func startTmux(id, shell string, cols, rows uint16, dir string) (*PTY, error) {
	if !tmuxAvailable() {
		return nil, fmt.Errorf("tmux is not installed")
	}
	name := tmuxName(id)
	// The status bar is a row of a phone screen spent on a session name, a
	// host and a clock. It used to be switched off after the client had already
	// attached, which raced the first frame: the bar was painted once and
	// stayed at the bottom until something else redrew. Create the session
	// detached, turn that row off, and only then attach.
	if !tmuxHasSession(name) {
		args := []string{"-L", tmuxSocket, "new-session", "-d", "-s", name}
		if dir != "" {
			args = append(args, "-c", dir)
		}
		args = append(args, shell, "-l")
		if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
			// Another attach may have created it between the check and here.
			if !tmuxHasSession(name) {
				return nil, fmt.Errorf("create tmux session: %v: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	_ = exec.Command("tmux", "-L", tmuxSocket, "set-option", "-t", name, "status", "off").Run()
	return StartTmux(name, shell, cols, rows, dir)
}

func randomTicket() string {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

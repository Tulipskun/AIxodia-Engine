// Package kaggle keeps a Kaggle notebook session's lifetime bounded.
//
// A Kaggle machine is ephemeral: when the notebook cell returns, the session is
// gone, and with it /root, /opt, /etc, /var and /usr. The notebook itself only
// restores that machine, starts the engine and waits for it to exit; it decides
// nothing. This package owns the session instead: when the operator drops
// ~/.exit (or the notebook's 11.5 h budget is spent) it snapshots the machine
// through kgat_sync.py, pushes a fresh kernel version, and reports that the
// engine should stop — which ends the cell and with it the session.
package kaggle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Config is one session supervisor.
type Config struct {
	Script   string        // kgat_sync.py: restore, backup, restart
	Python   string        // interpreter; "python3" when empty
	ExitFile string        // the operator's ~/.exit; empty means $HOME/.exit
	Limit    time.Duration // how long a session may run; 41400s when zero
	Grace    time.Duration // extra time to finish after Limit; 20m when zero
	Every    time.Duration // how often ExitFile is checked; 10s when zero
	Retry    time.Duration // wait after a failed step; 5m when zero
	Now      func() time.Time
	// Exec runs one script action. It exists so a test can watch the order of
	// backup and restart without a real snapshot; nil runs the script.
	Exec func(ctx context.Context, script, action string) error
	Logf func(format string, args ...any)
}

// Result describes how a session ended.
type Result struct {
	// Reason is what ended it: "exit file" or "session limit".
	Reason string
	// BackedUp reports that the snapshot was uploaded before the restart.
	BackedUp bool
}

// Run watches the session and, once it is over, snapshots the machine and
// pushes a fresh kernel version. It returns nil only once the restart has been
// pushed; a cancelled context or a failure that could not be retried before the
// hard deadline returns that error instead.
func (c Config) Run(ctx context.Context) (Result, error) {
	c = c.defaults()
	res := Result{}
	start := c.Now()
	hard := start.Add(c.Limit + c.Grace)
	backedUp := false

	tick := time.NewTicker(c.Every)
	defer tick.Stop()

	for {
		if res.Reason == "" {
			res.Reason = c.due(start)
			if res.Reason != "" && c.ExitFile != "" {
				// Removing it before the snapshot keeps the file out of the
				// backup, so the next session does not exit immediately.
				if err := os.Remove(c.ExitFile); err == nil {
					c.Logf("session: removed %s", c.ExitFile)
				}
			}
		}
		if res.Reason == "" {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-tick.C:
			}
			continue
		}

		var err error
		if !backedUp {
			c.Logf("session: %s — snapshotting the machine", res.Reason)
			err = c.Exec(ctx, c.Script, "backup")
			if err == nil {
				backedUp, res.BackedUp = true, true
				c.Logf("session: snapshot finished")
			}
		} else {
			c.Logf("session: pushing a fresh kernel version")
			err = c.Exec(ctx, c.Script, "restart")
			if err == nil {
				c.Logf("session: restart pushed")
				return res, nil
			}
		}
		if err == nil {
			continue
		}
		if c.Now().Add(c.Retry).After(hard) {
			return res, err
		}
		c.Logf("session: %v — retrying in %s", err, c.Retry)
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(c.Retry):
		}
	}
}

func (c Config) defaults() Config {
	if c.Python == "" {
		c.Python = "python3"
	}
	if c.ExitFile == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.ExitFile = filepath.Join(home, ".exit")
		}
	}
	if c.Limit <= 0 {
		c.Limit = 41400 * time.Second
	}
	if c.Grace <= 0 {
		c.Grace = 20 * time.Minute
	}
	if c.Every <= 0 {
		c.Every = 10 * time.Second
	}
	if c.Retry <= 0 {
		c.Retry = 5 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Exec == nil {
		c.Exec = scriptRunner(c.Python, c.Logf)
	}
	return c
}

// due reports why the session is over, or "" while it may keep running.
func (c Config) due(start time.Time) string {
	if c.ExitFile != "" {
		if _, err := os.Stat(c.ExitFile); err == nil {
			return "exit file"
		}
	}
	if c.Limit > 0 && c.Now().Sub(start) >= c.Limit {
		return "session limit"
	}
	return ""
}

// scriptRunner executes the script and copies its output into the engine log,
// one prefixed line at a time. A snapshot prints hundreds of lines and they are
// worth having when a session fails to come back.
func scriptRunner(python string, logf func(string, ...any)) func(context.Context, string, string) error {
	return func(ctx context.Context, script, action string) error {
		cmd := exec.CommandContext(ctx, python, script, action)
		out := &lineWriter{logf: logf}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %w", filepath.Base(script), action, err)
		}
		return nil
	}
}

// lineWriter logs complete lines as they are written.
type lineWriter struct {
	buf  bytes.Buffer
	logf func(string, ...any)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Not a full line yet: keep the bytes for the next write.
			if line != "" {
				w.buf.WriteString(line)
			}
			break
		}
		w.logf("session: %s", trimLine(line))
	}
	return len(p), nil
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

var _ io.Writer = (*lineWriter)(nil)

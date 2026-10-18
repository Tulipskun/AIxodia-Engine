// Package terminal serves the shell of the machine the engine runs on over a
// WebSocket, so the phone can use that machine the way it would use a terminal
// app. The pty plumbing here is Linux only — the ioctls are Linux's — which is
// where the engine runs (Kaggle, and install.sh's hosts).
package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// PTY is a shell running on a pseudo-terminal, holding the master side. Every
// pty owns one shell: closing it ends that shell and nothing else.
type PTY struct {
	master *os.File
	cmd    *exec.Cmd

	waitOnce sync.Once
	code     int
}

// StartPTY opens a pty and starts shell on it, sized to cols x rows. dir is the
// shell's working directory, or "" to inherit this process's. The caller owns
// the result and must Close it.
func StartPTY(shell string, cols, rows uint16, dir string) (*PTY, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// The slave cannot be opened until the master is unlocked, and the number
	// after the slash is the kernel's to choose.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("read pty number: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open pty slave: %w", err)
	}

	cmd := exec.Command(shell)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	// Setsid makes the shell a session and process-group leader; Setctty with
	// Ctty 0 — its own stdin — makes the slave its controlling terminal, which
	// is what gives the shell job control and makes Ctrl-C mean anything.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	started := cmd.Start()
	// The child holds its own copy of the slave now. Keeping ours open would
	// stop the master from ever learning that the shell is gone.
	slave.Close()
	if started != nil {
		master.Close()
		return nil, fmt.Errorf("start %s: %w", shell, started)
	}

	p := &PTY{master: master, cmd: cmd}
	if err := p.Resize(cols, rows); err != nil {
		p.Close()
		return nil, fmt.Errorf("size pty: %w", err)
	}
	return p, nil
}

// Read and Write are the master side of the pty: what the shell printed, and
// what the keyboard sent.
func (p *PTY) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *PTY) Write(b []byte) (int, error) { return p.master.Write(b) }

// Resize sets the pty's window size. The kernel signals SIGWINCH to the
// shell's foreground process group by itself, so nothing here has to.
func (p *PTY) Resize(cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// Wait reaps the shell and returns its exit code, or -1 when a signal killed
// it. It is safe to call before or after Close, and more than once.
func (p *PTY) Wait() int {
	p.waitOnce.Do(func() {
		_ = p.cmd.Wait()
		p.code = -1
		if p.cmd.ProcessState != nil {
			p.code = p.cmd.ProcessState.ExitCode()
		}
	})
	return p.code
}

// Close hangs the pty up — the kernel signals the shell — and, while the shell
// is still there, kills its whole process group: a shell that ignores SIGHUP
// would otherwise leave its children running after the phone is gone. Closing
// the master is also what unblocks a reader parked on it. It reaps the shell,
// so nothing is left as a zombie, and it is safe to call twice.
func (p *PTY) Close() error {
	_ = p.master.Close()
	if p.cmd.Process != nil && p.cmd.ProcessState == nil {
		// A negative pid is the process group, which Setsid made the shell
		// lead. It is safe while the shell is unreaped, because its group id
		// is still reserved.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = p.Wait()
	return nil
}

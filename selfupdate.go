package main

// Self-update: the daemon replaces its own binary and re-executes, so a new
// engine build reaches a running kernel without a dataset version and without
// a kernel restart. syscall.Exec is what makes it safe here — it keeps the
// process id and the open stdout, so the notebook tailing this process never
// sees it stop, and the Kaggle session is not ended.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// engineReleaseURL is the rolling release the GitHub workflow republishes on
// every push to main. AIXODIA_ENGINE_URL overrides it for testing.
const engineReleaseURL = "https://github.com/Tulipskun/ai-engine/releases/download/engine-latest/ai-engine"

// minEngineBytes guards against a truncated or error-page download being
// executed: a real build is several megabytes.
const minEngineBytes = 2 << 20

var (
	tunnelMu  sync.Mutex
	tunnelCmd *exec.Cmd
)

// rememberTunnel keeps the cloudflared process so an update can stop it before
// the binary is replaced; otherwise every update would leave one orphaned.
func rememberTunnel(cmd *exec.Cmd) {
	tunnelMu.Lock()
	tunnelCmd = cmd
	tunnelMu.Unlock()
}

func killTunnel() {
	tunnelMu.Lock()
	cmd := tunnelCmd
	tunnelCmd = nil
	tunnelMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// selfUpdate downloads the newest engine and replaces this process with it.
// It returns an error only when the download or the verification failed, in
// which case nothing has been touched and the daemon keeps running as it was.
func selfUpdate() error {
	src := os.Getenv("AIXODIA_ENGINE_URL")
	if src == "" {
		src = engineReleaseURL
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(src)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	// A fresh path each time: Linux refuses to overwrite a file that is being
	// executed ("text file busy"), and the previous update's file is exactly
	// that.
	dst := fmt.Sprintf("%s.engine-update-%d", engineBinaryPath(), os.Getpid())
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	written, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("write: %v/%v", err, closeErr)
	}
	if written < minEngineBytes {
		_ = os.Remove(dst)
		return fmt.Errorf("downloaded only %d bytes — not an engine build", written)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("chmod: %w", err)
	}

	log.Printf("self-update: %d bytes ready at %s; restarting into it", written, dst)
	// The tunnel is stopped first so the replacement can open its own without
	// leaving the old one behind. The new process publishes the new URL.
	killTunnel()
	if err := syscall.Exec(dst, os.Args, os.Environ()); err != nil {
		// Exec failed, so this process is still here — but without a tunnel.
		// Say so loudly; the next kernel restart recovers.
		return fmt.Errorf("exec %s: %w (tunnel stopped)", dst, err)
	}
	return nil // unreachable: the process image was replaced
}

// engineBinaryPath is the running executable, used to place the update next to
// it so a self-exec has a predictable path.
func engineBinaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "/kaggle/working/ai-engine"
}

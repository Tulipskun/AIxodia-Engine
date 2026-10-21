package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// childEnv marks the re-executed copy of this test binary. TestMain runs the
// child body instead of the suite.
const childEnv = "AIXODIA_RUNNING_IMAGE_CHILD"

// TestMain lets the test binary serve as the "engine" that gets replaced: when
// the marker is set it runs the child body and exits, instead of the suite.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		os.Exit(childRunningImageTest())
	}
	os.Exit(m.Run())
}

// The bug this pins: install.sh renames a new binary over the running one, so
// the path this process was started from now holds *different* bytes. Hashing
// os.Executable() would then hash the new file, conclude the published build is
// already running, and never exec — the engine silently stays on the old build
// and even records the release as applied.
func TestCurrentSHA256ReadsTheRunningImage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the replaced-binary case is Linux-specific (/proc/self/exe)")
	}

	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	// A copy of this test binary stands in for the installed engine; the child
	// runs from it so that its /proc/self/exe is that path.
	engine := filepath.Join(t.TempDir(), "aixodia-engine")
	if err := os.WriteFile(engine, self, 0o755); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	sentinel := filepath.Join(dir, "replaced")
	replacement := []byte("#!/bin/sh\necho the published build\n")
	sum := sha256.Sum256(replacement)

	cmd := exec.Command(engine)
	cmd.Env = append(os.Environ(),
		childEnv+"=1",
		"SELFUPDATE_CHILD_READY="+ready,
		"SELFUPDATE_CHILD_WAIT="+sentinel,
		"SELFUPDATE_CHILD_NEW_SHA="+hex.EncodeToString(sum[:]),
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Replace the binary only once the child has hashed itself, so the first
	// hash is definitely the bytes it is running from.
	waitFor(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, "the child to become ready")
	// Rename over the path, the way install.sh's `mv -f` does: writing into the
	// file that is being executed fails with ETXTBSY, which is exactly why the
	// installer swaps by rename.
	tmp := engine + ".new"
	if err := os.WriteFile(tmp, replacement, 0o755); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if err := os.Rename(tmp, engine); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out.String())
	}
}

// childRunningImageTest is the child half: it hashes itself, tells the parent
// it is ready, waits for the binary at its own path to be replaced, and hashes
// itself again. A hash that changed means it followed the path instead of the
// image it is running.
func childRunningImageTest() int {
	if img := runningImage(); img != "/proc/self/exe" {
		fmt.Printf("runningImage() = %q, want /proc/self/exe\n", img)
		return 1
	}
	before, err := currentSHA256()
	if err != nil {
		fmt.Printf("first hash: %v\n", err)
		return 1
	}
	if err := os.WriteFile(os.Getenv("SELFUPDATE_CHILD_READY"), nil, 0o644); err != nil {
		fmt.Printf("ready marker: %v\n", err)
		return 1
	}

	wait := os.Getenv("SELFUPDATE_CHILD_WAIT")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(wait); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("the parent never replaced the binary")
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}

	after, err := currentSHA256()
	if err != nil {
		fmt.Printf("second hash: %v\n", err)
		return 1
	}
	if before != after {
		fmt.Printf("hash followed the replaced file: %s -> %s\n", before, after)
		return 1
	}
	if newSHA := os.Getenv("SELFUPDATE_CHILD_NEW_SHA"); after == newSHA {
		fmt.Printf("hash is the replacement build (%s), not the running one\n", after)
		return 1
	}
	return 0
}

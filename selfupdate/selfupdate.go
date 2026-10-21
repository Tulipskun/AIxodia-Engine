// Package selfupdate lets a running engine replace its own binary and restart
// without ending the host session. It compares the running executable against
// the published release asset and, when they differ, downloads, verifies by
// sha256 against the running copy, swaps and execs the new binary in place.
// `aixodia update` uses the same download through Install, and lets the
// launcher decide when the engine restarts.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const releaseURL = "https://api.github.com/repos/Tulipskun/aixodia-engine/releases/tags/engine-latest"

type Release struct {
	URL       string
	UpdatedAt string
	Size      int64
}

type ghAsset struct {
	Name      string `json:"name"`
	URL       string `json:"browser_download_url"`
	UpdatedAt string `json:"updated_at"`
	Size      int64  `json:"size"`
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

func assetName() string {
	if runtime.GOARCH == "arm64" {
		return "aixodia-engine-linux-arm64"
	}
	return "aixodia-engine-linux-amd64"
}

func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", releaseURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "aixodia-engine/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Release{}, fmt.Errorf("github release: %s", resp.Status)
	}
	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Release{}, err
	}
	name := assetName()
	for _, a := range r.Assets {
		if a.Name == name {
			return Release{URL: a.URL, UpdatedAt: a.UpdatedAt, Size: a.Size}, nil
		}
	}
	return Release{}, fmt.Errorf("asset %s not found", name)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func currentSHA256() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return sha256File(exe)
}

func statePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aixodia", "selfupdate.state")
}

func readState() string {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeState(v string) {
	dir := filepath.Dir(statePath())
	os.MkdirAll(dir, 0o755)
	os.WriteFile(statePath(), []byte(v+"\n"), 0o644)
}

// NeedUpdate reports whether the published asset differs from the running copy.
// It uses the asset's updated_at as a cheap change signal and confirms by
// sha256 of the downloaded bytes.
func NeedUpdate(ctx context.Context) (bool, Release, error) {
	rel, err := Latest(ctx)
	if err != nil {
		return false, rel, err
	}
	if rel.UpdatedAt == "" || rel.UpdatedAt == readState() {
		return false, rel, nil
	}
	return true, rel, nil
}

// Install downloads rel into target and swaps it into place, unless the
// published bytes are the running executable's. It reports whether target now
// holds a different binary.
func Install(ctx context.Context, rel Release, target string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rel.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "aixodia-engine/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("download: %s", resp.Status)
	}
	tmp := target + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, err
	}
	f.Close()
	got := hex.EncodeToString(h.Sum(nil))

	cur, _ := currentSHA256()
	if cur != "" && got == cur {
		os.Remove(tmp)
		writeState(rel.UpdatedAt)
		return false, nil // same binary, nothing to do
	}

	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return false, err
	}
	writeState(rel.UpdatedAt)
	return true, nil
}

// Apply installs rel and, when the binary changed, replaces this process with
// it: same pid, same arguments, same environment.
func Apply(ctx context.Context, rel Release, target string) error {
	changed, err := Install(ctx, rel, target)
	if err != nil || !changed {
		return err
	}
	return syscall.Exec(target, os.Args, os.Environ())
}

func TargetPath() string {
	if st, err := os.Stat("/usr/local/bin/aixodia-engine"); err == nil && !st.IsDir() {
		return "/usr/local/bin/aixodia-engine"
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "/usr/local/bin/aixodia-engine"
}

// RunLoop checks the published build every ten minutes, once the engine has had
// two minutes to finish starting, and installs it when it changed and no chat
// is in flight. A check that fails repeats every tick, so it is logged when the
// error changes, not on every attempt.
func RunLoop(ctx context.Context, idle func() bool) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()

	var lastErr string
	note := func(err error) {
		if err == nil {
			if lastErr != "" {
				log.Print("selfupdate: the release is reachable again")
				lastErr = ""
			}
			return
		}
		if msg := err.Error(); msg != lastErr {
			log.Printf("selfupdate: %v", err)
			lastErr = msg
		}
	}
	poll := func() {
		ok, rel, err := NeedUpdate(ctx)
		note(err)
		if err != nil || !ok {
			return
		}
		log.Printf("selfupdate: installing the build published %s", rel.UpdatedAt)
		// On success this does not return: the new binary takes over.
		note(Apply(ctx, rel, TargetPath()))
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	for {
		if idle == nil || idle() {
			poll()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

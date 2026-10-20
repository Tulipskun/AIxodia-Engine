// Package selfupdate lets a running engine replace its own binary and restart
// without ending the host session (e.g. a Kaggle notebook). It compares the
// running executable against the published release asset by sha256 and, when
// they differ, downloads, verifies, swaps and execs the new binary in place.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	URL    string
	Digest string
}

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

func assetName() string {
	arch := runtime.GOARCH
	if arch == "arm64" {
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
			return Release{URL: a.URL, Digest: a.Digest}, nil
		}
	}
	return Release{}, fmt.Errorf("asset %s not found", name)
}

func currentSHA256() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
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

// NeedUpdate reports whether the running binary differs from the published one.
func NeedUpdate(ctx context.Context) (bool, Release, error) {
	rel, err := Latest(ctx)
	if err != nil {
		return false, rel, err
	}
	remote := strings.TrimPrefix(rel.Digest, "sha256:")
	if remote == "" {
		return false, rel, nil
	}
	cur, err := currentSHA256()
	if err != nil {
		return false, rel, err
	}
	return remote != cur, rel, nil
}

// Apply downloads rel, verifies it, swaps it into target and execs it in place.
func Apply(ctx context.Context, rel Release, target string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", rel.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "aixodia-engine/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: %s", resp.Status)
	}
	tmp := target + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	got := hex.EncodeToString(h.Sum(nil))
	remote := strings.TrimPrefix(rel.Digest, "sha256:")
	if remote != "" && got != remote {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch: %s != %s", got, remote)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	// replace this process with the new binary
	return syscall.Exec(target, os.Args, os.Environ())
}

// TargetPath picks where to install the update.
func TargetPath() string {
	if st, err := os.Stat("/usr/local/bin/aixodia-engine"); err == nil && !st.IsDir() {
		return "/usr/local/bin/aixodia-engine"
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "/usr/local/bin/aixodia-engine"
}

// RunLoop checks for updates periodically and applies them when idle.
func RunLoop(ctx context.Context, idle func() bool) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	// first check shortly after start
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	for {
		if idle == nil || idle() {
			if ok, rel, err := NeedUpdate(ctx); err == nil && ok {
				_ = Apply(ctx, rel, TargetPath())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

var _ = filepath.Base

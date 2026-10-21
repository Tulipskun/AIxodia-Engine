package selfupdate

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The published bytes land in the target, and the asset's updated_at is what
// stops the next check from downloading them again.
func TestInstallWritesThePublishedBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := []byte("#!/bin/sh\necho new\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "aixodia-engine")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rel := Release{URL: srv.URL, UpdatedAt: "2026-10-21T00:00:00Z"}
	changed, err := Install(context.Background(), rel, target)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a different binary should report a change")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("target holds %q, want %q", got, payload)
	}
	if mode := fileInfo(t, target).Mode().Perm(); mode != 0o755 {
		t.Errorf("mode = %o, want 755", mode)
	}
	if state := readState(); state != rel.UpdatedAt {
		t.Errorf("state = %q, want %q", state, rel.UpdatedAt)
	}
}

// A download that fails leaves the running binary alone.
func TestInstallKeepsTheBinaryWhenTheDownloadFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "aixodia-engine")
	old := []byte("#!/bin/sh\necho old\n")
	if err := os.WriteFile(target, old, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), Release{URL: srv.URL}, target); err == nil {
		t.Fatal("a 404 should fail the install")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("target changed to %q", got)
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Error("a partial download was left behind")
	}
}

func fileInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

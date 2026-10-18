package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// POST /restart is how the phone ends a session: the notebook watches ~/.exit,
// backs the filesystem up and starts the next run. Nothing goes to D1.
func TestRestartWritesExitFileInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir reads this on unix
	gateway, _ := newTestGateway(t, newMemStore())

	recorder := postJSON(t, gateway.Handler(), "/restart", "{}")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".exit")); err != nil {
		t.Fatalf("no ~/.exit after POST /restart: %v", err)
	}
}

package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aixodia-engine/localdb"
)

// Settings are the phone's choice of model, saved between sessions. They used
// to be a D1 row and are now a key in the engine's own file; a fresh install
// still has to be handed the defaults rather than a failure.
func TestSettingsRoundTripLandsInTheStore(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })

	gateway, _ := newTestGateway(t, newMemStore())
	gateway.SetLocalStore(handle)
	handler := gateway.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != string(defaultSettings()) {
		t.Fatalf("settings before any write = %s, want the defaults", got)
	}

	saved := `{"main":{"provider":"Opencode","model":"big-pickle","generation":{}},"sub":{"provider":"","model":"","generation":{}},"sub_enabled":false}`
	put := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(saved))
	put.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, put)
	if recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != saved {
		t.Fatalf("settings after the write = %d %s, want the saved value", recorder.Code, recorder.Body.String())
	}

	stored, found, err := localdb.GetState(handle, settingsStateKey)
	if err != nil || !found {
		t.Fatalf("stored settings = %q, %v, %v; want the value in the file", stored, found, err)
	}
	if stored != saved {
		t.Fatalf("stored settings = %s, want %s", stored, saved)
	}
}

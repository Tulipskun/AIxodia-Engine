package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeState is D1's system_state for these tests: a map of key to value.
type fakeState struct {
	values map[string]string
}

func (f *fakeState) GetState(key string) (string, bool, error) {
	value, ok := f.values[key]
	return value, ok, nil
}

func (f *fakeState) PutState(key, value string) error {
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}

// Settings are the phone's choice of model, saved between sessions. They live
// in D1's system_state now; a fresh install still has to be handed the defaults
// rather than a failure.
func TestSettingsRoundTripLandsInTheStore(t *testing.T) {
	state := &fakeState{values: map[string]string{}}
	gateway, _ := newTestGateway(t, newMemStore())
	gateway.SetStateStore(state)
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

	stored, found := state.values[settingsStateKey]
	if !found {
		t.Fatalf("stored settings missing, want the value in the state store")
	}
	if stored != saved {
		t.Fatalf("stored settings = %s, want %s", stored, saved)
	}
}

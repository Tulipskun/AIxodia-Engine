package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ai-engine/localdb"
	"ai-engine/session"
)

// Pinning a model for one chat is what the phone's per-chat menu does. The pin
// lives in the local store, and a patch that names one field must leave the
// others — including the provider and model the chat runs on — alone.
func TestSessionAgentPatchLandsInTheStore(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	store := session.NewSQLite(handle)
	gateway, _ := newTestGateway(t, store)
	handler := gateway.Handler()

	created := postJSON(t, handler, "/v1/sessions", `{"title":"hello","model":"mock/test-model"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdSession struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdSession); err != nil {
		t.Fatal(err)
	}

	patch := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+createdSession.ID, strings.NewReader(`{"sub_provider":"Other","sub_model":"small","sub_enabled":true}`))
	patch.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, patch)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		SubProvider string `json:"sub_provider"`
		SubModel    string `json:"sub_model"`
		SubEnabled  bool   `json:"sub_enabled"`
		SubPinned   bool   `json:"sub_pinned"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.SubProvider != "Other" || answer.SubModel != "small" || !answer.SubEnabled || !answer.SubPinned {
		t.Fatalf("patch answer = %+v, want the pinned sub agent", answer)
	}
	if answer.Provider != "" || answer.Model != "mock/test-model" {
		t.Fatalf("patch answer = %+v, want the chat's own model untouched", answer)
	}

	// The pin has to be in the file, not only in the answer.
	stored, err := store.Get(createdSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SubProvider != "Other" || stored.SubModel != "small" || !stored.SubEnabled {
		t.Fatalf("stored session = %+v, want the pin written to the store", stored)
	}
	if stored.Provider != "" || stored.Model != "mock/test-model" || stored.Title != "hello" {
		t.Fatalf("stored session = %+v, want the chat untouched otherwise", stored)
	}

	missing := httptest.NewRequest(http.MethodPatch, "/api/sessions/no-such-chat", strings.NewReader(`{"sub_model":"small"}`))
	missing.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, missing)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("patch of a missing session = %d, body = %s; want 404", recorder.Code, recorder.Body.String())
	}
}

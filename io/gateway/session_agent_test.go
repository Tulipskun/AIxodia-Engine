package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pinning a model for one chat is what the phone's per-chat menu does. The pin
// goes through the session store, and a patch that names one field must leave
// the others — including the provider and model the chat runs on — alone.
func TestSessionModelPatchLandsInTheStore(t *testing.T) {
	store := newMemStore()
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

	patch := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+createdSession.ID, strings.NewReader(`{"model":"pinned-model"}`))
	patch.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, patch)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Pinned   bool   `json:"pinned"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Model != "pinned-model" || !answer.Pinned {
		t.Fatalf("patch answer = %+v, want the pinned model", answer)
	}
	if answer.Provider != "" {
		t.Fatalf("patch answer = %+v, want the provider untouched", answer)
	}

	// The pin has to be in the store, not only in the answer.
	stored, err := store.Get(createdSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Model != "pinned-model" {
		t.Fatalf("stored session = %+v, want the pin written to the store", stored)
	}
	if stored.Provider != "" || stored.Title != "hello" {
		t.Fatalf("stored session = %+v, want the chat untouched otherwise", stored)
	}

	missing := httptest.NewRequest(http.MethodPatch, "/api/sessions/no-such-chat", strings.NewReader(`{"model":"small"}`))
	missing.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, missing)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("patch of a missing session = %d, body = %s; want 404", recorder.Code, recorder.Body.String())
	}

	// Reading the pin of a chat that is gone is a 404 too, not a store failure:
	// the phone asks for this after it deletes a chat.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sessions/no-such-chat", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("get of a missing session = %d, body = %s; want 404", recorder.Code, recorder.Body.String())
	}
}

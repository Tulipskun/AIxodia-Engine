package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"ai-engine/session"
)

// The live failure was this endpoint: GET /v1/sessions never answered because
// the SQLite store's List deadlocked on the pool's single connection. Every
// other gateway test runs on memStore, where that cannot happen, so the
// endpoint needs one test on the store the engine actually ships.
//
// The handler runs on a goroutine with a deadline instead of an httptest
// Server: a server's Close waits for the stuck request, so a regression would
// hang the test binary instead of failing it.
func TestListSessionsAnswersOnTheSQLiteStore(t *testing.T) {
	store, err := session.NewSQLite(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, _ := newTestGateway(t, store)

	created := postJSON(t, gateway.Handler(), "/v1/sessions", `{"title":"hello","model":"mock/test-model"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
		answered <- recorder
	}()

	select {
	case recorder := <-answered:
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		var payload struct {
			Sessions []struct {
				Title string `json:"title"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %q: %v", recorder.Body.String(), err)
		}
		if len(payload.Sessions) != 1 || payload.Sessions[0].Title != "hello" {
			t.Fatalf("sessions = %+v, want one titled hello", payload.Sessions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GET /v1/sessions never answered: the store is waiting forever for its only connection")
	}
}

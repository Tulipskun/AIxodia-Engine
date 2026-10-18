package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aixodia-engine/config"
	"aixodia-engine/localdb"
	"aixodia-engine/session"
)

// The provider API is where the API keys are edited, and the keys are the one
// thing the engine cannot recreate. This drives the real store rather than a
// mock: what the handler writes has to be in the file, which is why the checks
// read it back through a second store over the same handle.
func TestProviderAPIRoundTripLandsInTheStore(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	config.SetStore(config.NewStore(handle))
	t.Cleanup(func() { config.SetStore(nil) })

	store := session.NewSQLite(handle)
	gateway, _ := newTestGateway(t, store)
	handler := gateway.Handler()

	created := postJSON(t, handler, "/api/providers", `{"id":"Probe","endpoint":"https://probe.test","adapter":"openai","keys":["k1"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("add status = %d, body = %s", created.Code, created.Body.String())
	}

	// Read through a second store: the handler's own copy could be in memory.
	fromFile, err := config.NewStore(handle).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile) != 1 || fromFile[0].Name != "Probe" || fromFile[0].APIURL != "https://probe.test" || fromFile[0].Adapter != "openai" {
		t.Fatalf("stored providers = %+v, want the provider that was added", fromFile)
	}
	if !reflect.DeepEqual(fromFile[0].Keys, []string{"k1"}) {
		t.Fatalf("stored keys = %v, want k1", fromFile[0].Keys)
	}

	keys := postJSON(t, handler, "/api/providers/Probe/keys", `{"add":["k2"]}`)
	if keys.Code != http.StatusOK {
		t.Fatalf("keys status = %d, body = %s", keys.Code, keys.Body.String())
	}
	fromFile, err = config.NewStore(handle).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile) != 1 || !reflect.DeepEqual(fromFile[0].Keys, []string{"k1", "k2"}) {
		t.Fatalf("stored keys after the change = %+v, want k1 and k2", fromFile)
	}

	remove := httptest.NewRequest(http.MethodDelete, "/api/providers/Probe", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, remove)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	fromFile, err = config.NewStore(handle).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile) != 0 {
		t.Fatalf("stored providers after the remove = %+v, want none", fromFile)
	}
}

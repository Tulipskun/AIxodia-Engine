package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aixodia-engine/config"
)

// providerKV is the KV namespace the provider API writes to, kept in a map.
type providerKV struct {
	values map[string]string
}

func (f *providerKV) Get(key string) (string, bool, error) {
	value, ok := f.values[key]
	return value, ok, nil
}

func (f *providerKV) Put(key, value string) error {
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}

func testProviderAPI(t *testing.T) (*providerKV, http.Handler) {
	t.Helper()
	kv := &providerKV{values: map[string]string{}}
	config.SetStore(config.NewStore(kv))
	t.Cleanup(func() { config.SetStore(nil) })

	gateway, _ := newTestGateway(t, newMemStore())
	return kv, gateway.Handler()
}

// The provider API is where the API keys are edited, and the keys are the one
// thing the engine cannot recreate. This drives the real store rather than a
// mock: what the handler writes has to be in KV, which is why the checks read
// it back through a second store over the same values.
func TestProviderAPIRoundTripLandsInTheStore(t *testing.T) {
	kv, handler := testProviderAPI(t)

	created := postJSON(t, handler, "/api/providers", `{"id":"Probe","endpoint":"https://probe.test","adapter":"openai","keys":["k1"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("add status = %d, body = %s", created.Code, created.Body.String())
	}

	fromStore, err := config.NewStore(kv).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromStore) != 1 || fromStore[0].Name != "Probe" || fromStore[0].APIURL != "https://probe.test" || fromStore[0].Adapter != "openai" {
		t.Fatalf("stored providers = %+v, want the provider that was added", fromStore)
	}
	if !reflect.DeepEqual(fromStore[0].Keys, []string{"k1"}) {
		t.Fatalf("stored keys = %v, want k1", fromStore[0].Keys)
	}

	keys := postJSON(t, handler, "/api/providers/Probe/keys", `{"add":["k2"]}`)
	if keys.Code != http.StatusOK {
		t.Fatalf("keys status = %d, body = %s", keys.Code, keys.Body.String())
	}
	fromStore, err = config.NewStore(kv).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromStore) != 1 || !reflect.DeepEqual(fromStore[0].Keys, []string{"k1", "k2"}) {
		t.Fatalf("stored keys after the change = %+v, want k1 and k2", fromStore)
	}

	remove := httptest.NewRequest(http.MethodDelete, "/api/providers/Probe", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, remove)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	fromStore, err = config.NewStore(kv).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromStore) != 0 {
		t.Fatalf("stored providers after the remove = %+v, want none", fromStore)
	}
}

// The built-in opencode provider is not a KV entry, so removing it through the
// API has to be refused rather than deleting nothing and reporting success.
func TestProviderAPIRefusesToRemoveTheBuiltin(t *testing.T) {
	_, handler := testProviderAPI(t)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/providers/Opencode", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("remove builtin status = %d, want a refusal; body = %s", recorder.Code, recorder.Body.String())
	}
}

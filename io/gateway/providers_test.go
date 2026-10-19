package gateway

import (
	"fmt"
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
	d1 := &fakeProviders{}
	config.SetStore(config.NewStore(d1.query))
	t.Cleanup(func() { config.SetStore(nil) })

	store := session.NewSQLite(handle)
	gateway, _ := newTestGateway(t, store)
	handler := gateway.Handler()

	created := postJSON(t, handler, "/api/providers", `{"id":"Probe","endpoint":"https://probe.test","adapter":"openai","keys":["k1"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("add status = %d, body = %s", created.Code, created.Body.String())
	}

	// Read through a second store: the handler's own copy could be in memory.
	fromStore, err := config.NewStore(d1.query).List()
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
	fromStore, err = config.NewStore(d1.query).List()
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
	fromStore, err = config.NewStore(d1.query).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromStore) != 0 {
		t.Fatalf("stored providers after the remove = %+v, want none", fromStore)
	}
}

// The built-in opencode provider is not a D1 row, so removing it through the
// API has to be refused rather than deleting nothing and reporting success.
func TestProviderAPIRefusesToRemoveTheBuiltin(t *testing.T) {
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	config.SetStore(config.NewStore((&fakeProviders{}).query))
	t.Cleanup(func() { config.SetStore(nil) })
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	gateway, _ := newTestGateway(t, session.NewSQLite(handle))
	request := httptest.NewRequest(http.MethodDelete, "/api/providers/Opencode", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("remove builtin status = %d, want a refusal; body = %s", recorder.Code, recorder.Body.String())
	}
}

// fakeProviders is the D1 side of the provider table for these tests: the
// handlers write through config.Store, and these rows are what a later read
// finds. Numbers arrive as float64 and keys as a JSON string, as D1 does.
type fakeProviders struct {
	rows []map[string]any
	next int
}

func (f *fakeProviders) query(sql string, params ...any) ([]map[string]any, error) {
	switch {
	case strings.HasPrefix(sql, "SELECT \"index\""):
		return f.rows, nil
	case strings.HasPrefix(sql, "SELECT COUNT(*)"):
		return []map[string]any{{"n": float64(len(f.rows))}}, nil
	case strings.HasPrefix(sql, "INSERT INTO llm_providers"):
		f.next++
		f.rows = append(f.rows, map[string]any{
			"idx":      float64(f.next),
			"provider": params[0],
			"endpoint": params[1],
			"keys":     params[2],
			"adapter":  params[3],
		})
		return nil, nil
	case strings.HasPrefix(sql, "DELETE FROM llm_providers"):
		name := params[0].(string)
		kept := f.rows[:0]
		for _, row := range f.rows {
			if row["provider"] != name {
				kept = append(kept, row)
			}
		}
		f.rows = kept
		return nil, nil
	case strings.HasPrefix(sql, "UPDATE llm_providers"):
		name := params[1].(string)
		for _, row := range f.rows {
			if row["provider"] == name {
				row["keys"] = params[0]
			}
		}
		return nil, nil
	case strings.HasPrefix(sql, "CREATE TABLE"):
		return nil, nil
	default:
		return nil, fmt.Errorf("fakeProviders: unexpected SQL %q", sql)
	}
}

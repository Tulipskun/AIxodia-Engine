package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeD1 answers the handful of statements the store sends, keeping the rows in
// memory. It is the shape of D1 that matters here: values come back as
// []map[string]any with numbers as float64, because that is what the real
// client decodes.
type fakeD1 struct {
	rows   []map[string]any
	nextID int
	err    error
	seen   []string
}

func (f *fakeD1) query(sql string, params ...any) ([]map[string]any, error) {
	f.seen = append(f.seen, strings.Fields(sql)[0])
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.HasPrefix(sql, "SELECT \"index\""):
		return f.rows, nil
	case strings.HasPrefix(sql, "SELECT COUNT(*)"):
		return []map[string]any{{"n": float64(len(f.rows))}}, nil
	case strings.HasPrefix(sql, "INSERT INTO llm_providers"):
		f.nextID++
		f.rows = append(f.rows, map[string]any{
			"idx":      float64(f.nextID),
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
		return nil, fmt.Errorf("fakeD1: unexpected SQL %q", sql)
	}
}

func testStore(t *testing.T) (*Store, *fakeD1) {
	t.Helper()
	fake := &fakeD1{}
	store := NewStore(fake.query)
	SetStore(store)
	mu.Lock()
	Providers = nil
	mu.Unlock()
	t.Cleanup(func() {
		SetStore(nil)
		mu.Lock()
		Providers = nil
		mu.Unlock()
	})
	return store, fake
}

// A provider is a name, an endpoint, an adapter and its keys. Losing any of
// them loses the ability to talk to that upstream.
func TestStoreRoundTrip(t *testing.T) {
	store, _ := testStore(t)
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai", Keys: []string{"k1", "k2"}}); err != nil {
		t.Fatal(err)
	}

	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("List = %+v, want one provider", got)
	}
	if got[0].ID == "" || got[0].Name != "Alpha" || got[0].APIURL != "https://alpha.test" || got[0].Adapter != "openai" {
		t.Fatalf("List = %+v, want the provider that was written", got[0])
	}
	if !reflect.DeepEqual(got[0].Keys, []string{"k1", "k2"}) {
		t.Fatalf("keys = %v, want k1 k2", got[0].Keys)
	}

	if err := store.Remove("Alpha"); err != nil {
		t.Fatal(err)
	}
	got, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("List after Remove = %+v, want none", got)
	}
}

func TestStoreSetKeysReplacesThem(t *testing.T) {
	store, _ := testStore(t)
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai", Keys: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKeys("Alpha", []string{"new", "second"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Keys, []string{"new", "second"}) {
		t.Fatalf("keys after SetKeys = %+v, want new second", got)
	}
}

func TestStoreCount(t *testing.T) {
	store, _ := testStore(t)
	count, err := store.Count()
	if err != nil || count != 0 {
		t.Fatalf("Count = %d, err = %v; want 0, nil", count, err)
	}
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai"}); err != nil {
		t.Fatal(err)
	}
	if count, err = store.Count(); err != nil || count != 1 {
		t.Fatalf("Count = %d, err = %v; want 1, nil", count, err)
	}
}

func TestStoreEnsureCreatesTheTable(t *testing.T) {
	_, fake := testStore(t)
	if err := EnsureStore(); err != nil {
		t.Fatal(err)
	}
	if len(fake.seen) == 0 || fake.seen[0] != "CREATE" {
		t.Fatalf("statements = %v, want CREATE first", fake.seen)
	}
}

// The engine carries the opencode free tier itself, so a phone with no D1 rows
// still has a provider - and it is read-only.
func TestLoadConfigAlwaysAddsTheBuiltin(t *testing.T) {
	store, _ := testStore(t)
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai", Keys: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	got := GetProviders()
	if len(got) != 2 || got[0].Name != "Alpha" || got[1].Name != "Opencode" {
		t.Fatalf("GetProviders = %+v, want Alpha then the built-in Opencode", got)
	}
	if _, ok := GetProviderByName("Opencode"); !ok {
		t.Fatal("GetProviderByName(Opencode) not found")
	}
}

func TestBuiltinProviderCannotBeChanged(t *testing.T) {
	testStore(t)
	if !IsBuiltin("Opencode") || !IsBuiltin("opencode") {
		t.Fatal("the built-in provider must be recognised by name, case-insensitively")
	}
	if err := AddProvider("Opencode", "https://elsewhere.test", "openai", []string{"k"}); err == nil {
		t.Fatal("AddProvider over a built-in must be refused")
	}
	if err := RemoveProvider("Opencode"); err == nil {
		t.Fatal("RemoveProvider over a built-in must be refused")
	}
	if err := SetProviderKeys("Opencode", []string{"k"}); err == nil {
		t.Fatal("SetProviderKeys over a built-in must be refused")
	}
}

func TestStoreReportsD1Failures(t *testing.T) {
	store, fake := testStore(t)
	fake.err = fmt.Errorf("db: D1 query failed: HTTP 502")
	if _, err := store.List(); err == nil {
		t.Fatal("List must report a D1 failure")
	}
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai"}); err == nil {
		t.Fatal("Add must report a D1 failure")
	}
}

// A boot that cannot reach D1 still has the free provider: it is carried by the
// engine, not by the store.
func TestLoadConfigKeepsTheBuiltinWhenD1Fails(t *testing.T) {
	_, fake := testStore(t)
	fake.err = fmt.Errorf("db: D1 query failed: HTTP 502")
	if err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig must report the D1 failure")
	}
	got := GetProviders()
	if len(got) != 1 || got[0].Name != "Opencode" {
		t.Fatalf("GetProviders = %+v, want the built-in only", got)
	}
}

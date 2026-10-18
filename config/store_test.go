package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"ai-engine/localdb"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	return NewStore(handle)
}

// A provider is a name, an endpoint, an adapter and its keys. Losing any of
// them loses the ability to talk to that upstream.
func TestStoreRoundTrip(t *testing.T) {
	store := testStore(t)
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
	store := testStore(t)
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

// The ID is D1's index column, and rows carried over keep theirs: a phone that
// remembers provider 3 must find provider 3.
func TestStoreImportKeepsTheIDsItIsGiven(t *testing.T) {
	store := testStore(t)
	if err := store.Import([]Provider{{ID: "7", Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai", Keys: []string{"k"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "7" {
		t.Fatalf("List = %+v, want provider with ID 7", got)
	}
}

func TestLoadConfigReadsTheStore(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai", Keys: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Provider{Name: "Beta", APIURL: "https://beta.test", Adapter: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	got := GetProviders()
	if len(got) != 2 || got[0].Name != "Alpha" || got[1].Name != "Beta" {
		t.Fatalf("GetProviders = %+v, want Alpha then Beta", got)
	}
	if _, ok := GetProviderByName("Beta"); !ok {
		t.Fatal("GetProviderByName(Beta) not found")
	}
}

// D1 still holds the keys until its table is dropped. The import is the one
// read the engine makes, and it must not run over a store that has providers.
func TestImportLegacyProvidersSkipsAStoreThatHasProviders(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })
	if err := store.Add(Provider{Name: "Local", APIURL: "https://local.test", Adapter: "openai"}); err != nil {
		t.Fatal(err)
	}

	called := false
	imported, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		called = true
		return nil, errors.New("must not be read")
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || imported != 0 {
		t.Fatalf("imported %d, read D1 = %v; want 0 and no read", imported, called)
	}
}

func TestImportLegacyProvidersCopiesD1Rows(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	rows := []map[string]any{
		{"index": float64(1), "provider": "Opencode", "endpoint": "https://opencode.test", "keys": `["sk-one"]`, "adapter": "opencode"},
		{"index": float64(2), "provider": "Anthropic", "endpoint": "https://anthropic.test", "keys": `["sk-two","sk-three"]`, "adapter": "anthropic"},
	}
	reads := 0
	query := func(string, ...any) ([]map[string]any, error) {
		reads++
		return rows, nil
	}

	imported, err := ImportLegacyProviders(query)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 2 {
		t.Fatalf("imported = %d, want 2", imported)
	}
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[0].Name != "Opencode" || got[1].ID != "2" {
		t.Fatalf("List = %+v, want the two D1 rows with their IDs", got)
	}
	if !reflect.DeepEqual(got[1].Keys, []string{"sk-two", "sk-three"}) {
		t.Fatalf("keys = %v, want the D1 keys", got[1].Keys)
	}

	// A second boot finds the store populated and reads nothing.
	imported, err = ImportLegacyProviders(query)
	if err != nil || imported != 0 || reads != 1 {
		t.Fatalf("second import = %d, reads = %d, err = %v; want 0, 1, nil", imported, reads, err)
	}
}

// Once D1's table is dropped the read fails with "no such table". That is the
// expected end state, not an error worth a log line every minute.
func TestImportLegacyProvidersIgnoresAMissingTable(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	imported, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		return nil, errors.New("db: D1 query failed: no such table: llm_providers")
	})
	if err != nil || imported != 0 {
		t.Fatalf("import = %d, err = %v; want 0, nil", imported, err)
	}

	// And it stops reading D1: the table is gone for good.
	reads := 0
	imported, err = ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		reads++
		return nil, nil
	})
	if err != nil || imported != 0 || reads != 0 {
		t.Fatalf("second import = %d, reads = %d, err = %v; want 0, 0, nil", imported, reads, err)
	}
}

// A phone that removes its last provider means it. While D1 still holds the
// table, the import must not hand them back.
func TestImportLegacyProvidersDoesNotResurrectRemovedProviders(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	rows := []map[string]any{{"index": float64(1), "provider": "Opencode", "endpoint": "https://opencode.test", "keys": `["sk-one"]`, "adapter": "opencode"}}
	if _, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) { return rows, nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("Opencode"); err != nil {
		t.Fatal(err)
	}

	reads := 0
	imported, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		reads++
		return rows, nil
	})
	if err != nil || imported != 0 || reads != 0 {
		t.Fatalf("import after removing everything = %d, reads = %d, err = %v; want 0, 0, nil", imported, reads, err)
	}
}

// D1's table can also be there but empty: that is the end of the copy too.
func TestImportLegacyProvidersStopsOnAnEmptyD1Table(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	if _, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	reads := 0
	imported, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		reads++
		return nil, nil
	})
	if err != nil || imported != 0 || reads != 0 {
		t.Fatalf("second import = %d, reads = %d, err = %v; want 0, 0, nil", imported, reads, err)
	}
}

// A read that failed is the one case that has to be tried again: the keys are
// still in D1 and the phone has none.
func TestImportLegacyProvidersRetriesWhileTheReadFails(t *testing.T) {
	store := testStore(t)
	SetStore(store)
	t.Cleanup(func() { SetStore(nil) })

	imported, err := ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) {
		return nil, errors.New("db: D1 query failed: HTTP 502")
	})
	if err == nil || imported != 0 {
		t.Fatalf("import = %d, err = %v; want the failure reported", imported, err)
	}

	rows := []map[string]any{{"index": float64(3), "provider": "Opencode", "endpoint": "https://opencode.test", "keys": `["sk-one"]`, "adapter": "opencode"}}
	imported, err = ImportLegacyProviders(func(string, ...any) ([]map[string]any, error) { return rows, nil })
	if err != nil || imported != 1 {
		t.Fatalf("second import = %d, err = %v; want 1, nil", imported, err)
	}
}

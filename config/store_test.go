package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// fakeKV is the KV namespace the store talks to, kept in a map. A key that was
// never written is reported as missing, exactly as Workers KV does.
type fakeKV struct {
	values map[string]string
	err    error
	puts   int
}

func (f *fakeKV) Get(key string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	value, ok := f.values[key]
	return value, ok, nil
}

func (f *fakeKV) Put(key, value string) error {
	if f.err != nil {
		return f.err
	}
	f.puts++
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}

func testStore(t *testing.T) (*Store, *fakeKV) {
	t.Helper()
	fake := &fakeKV{values: map[string]string{}}
	store := NewStore(fake)
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
	store, fake := testStore(t)
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
	if got[0].ID != "1" || got[0].Name != "Alpha" || got[0].APIURL != "https://alpha.test" || got[0].Adapter != "openai" {
		t.Fatalf("List = %+v, want the provider that was written", got[0])
	}
	if !reflect.DeepEqual(got[0].Keys, []string{"k1", "k2"}) {
		t.Fatalf("keys = %v, want k1 k2", got[0].Keys)
	}

	// The value is one JSON object under the providers key, the shape the rest
	// of the engine and the phone's tooling expect.
	raw := fake.values[providersKey]
	var file providerFile
	if err := json.Unmarshal([]byte(raw), &file); err != nil {
		t.Fatalf("stored value is not JSON: %v (%s)", err, raw)
	}
	if file.NextID != 2 || len(file.Providers) != 1 || file.Providers[0].Provider != "Alpha" {
		t.Fatalf("stored file = %+v, want one Alpha and next_id 2", file)
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

// A fresh namespace has no providers key at all, which is an empty list and not
// a failure: the engine must boot on the built-in provider.
func TestStoreWithNoKeyIsEmpty(t *testing.T) {
	store, _ := testStore(t)
	got, err := store.List()
	if err != nil {
		t.Fatalf("List on an empty namespace: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v, want none", got)
	}
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai"}); err != nil {
		t.Fatal(err)
	}
	got, err = store.List()
	if err != nil || len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("first provider = %+v, err = %v; want id 1", got, err)
	}
}

// The engine carries the opencode free tier itself, so a phone with no KV
// providers still has something to talk to - and it is read-only.
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

func TestStoreReportsKVFailures(t *testing.T) {
	store, fake := testStore(t)
	fake.err = fmt.Errorf("db: KV read failed: HTTP 502")
	if _, err := store.List(); err == nil {
		t.Fatal("List must report a KV failure")
	}
	if err := store.Add(Provider{Name: "Alpha", APIURL: "https://alpha.test", Adapter: "openai"}); err == nil {
		t.Fatal("Add must report a KV failure")
	}
}

// A boot that cannot reach KV still has the free provider: it is carried by the
// engine, not by the store.
func TestLoadConfigKeepsTheBuiltinWhenKVFails(t *testing.T) {
	_, fake := testStore(t)
	fake.err = fmt.Errorf("db: KV read failed: HTTP 502")
	if err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig must report the KV failure")
	}
	got := GetProviders()
	if len(got) != 1 || got[0].Name != "Opencode" {
		t.Fatalf("GetProviders = %+v, want the built-in only", got)
	}
}

func TestStoreRejectsCorruptValue(t *testing.T) {
	store, fake := testStore(t)
	fake.values[providersKey] = `{"providers": oops}`
	if _, err := store.List(); err == nil {
		t.Fatal("a corrupt providers value must be an error, not an empty list")
	}
}

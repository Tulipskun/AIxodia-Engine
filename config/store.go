package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// KV is the Workers KV namespace the providers live in. It is db.KV in
// production and a map in tests, so the store can be exercised without a
// network.
type KV interface {
	Get(key string) (string, bool, error)
	Put(key string, value string) error
}

// providersKey is the one key the store reads and writes. The whole list is a
// single value: adding or removing a provider is one atomic read-modify-write,
// and a config reload costs one KV read.
const providersKey = "providers"

// providerEntry is the shape of one provider in the stored JSON. The field
// names mirror the old D1 columns (`provider`, `endpoint`, `adapter`, `keys`)
// so the value is recognisable to anything that read that table.
type providerEntry struct {
	ID       int      `json:"id"`
	Provider string   `json:"provider"`
	Endpoint string   `json:"endpoint"`
	Keys     []string `json:"keys"`
	Adapter  string   `json:"adapter"`
}

// providerFile is the value stored under providersKey.
type providerFile struct {
	NextID    int             `json:"next_id"`
	Providers []providerEntry `json:"providers"`
}

// Store keeps the providers in KV. The API keys are the one thing here that
// cannot be recreated from anywhere else, so they travel with the provider they
// belong to and live off this machine: a fresh session still finds them.
type Store struct {
	kv KV
}

func NewStore(kv KV) *Store {
	return &Store{kv: kv}
}

var store *Store

// SetStore points the package at the store opened at boot. Providers are read
// from it on a timer and written by the API, so it is set once and never
// swapped.
func SetStore(s *Store) { store = s }

func current() (*Store, error) {
	if store == nil {
		return nil, fmt.Errorf("config: the provider store is not open")
	}
	return store, nil
}

// read returns the stored file. A key that was never written is an empty file,
// not an error: a fresh namespace has no providers yet.
func (s *Store) read() (providerFile, error) {
	raw, found, err := s.kv.Get(providersKey)
	if err != nil {
		return providerFile{}, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return providerFile{NextID: 1}, nil
	}

	var file providerFile
	if err := json.Unmarshal([]byte(raw), &file); err != nil {
		return providerFile{}, fmt.Errorf("config: providers in KV are not valid JSON: %w", err)
	}
	if file.Providers == nil {
		file.Providers = []providerEntry{}
	}
	// A file written before next_id existed, or one whose counter fell behind,
	// must not hand out an ID twice.
	lowest := 1
	for _, entry := range file.Providers {
		if entry.ID >= lowest {
			lowest = entry.ID + 1
		}
	}
	if file.NextID < lowest {
		file.NextID = lowest
	}
	return file, nil
}

func (s *Store) write(file providerFile) error {
	if file.Providers == nil {
		file.Providers = []providerEntry{}
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return s.kv.Put(providersKey, string(encoded))
}

// List returns the providers in stored order, which is the order the phone
// sees.
func (s *Store) List() ([]Provider, error) {
	file, err := s.read()
	if err != nil {
		return nil, err
	}
	providers := make([]Provider, 0, len(file.Providers))
	for _, entry := range file.Providers {
		providers = append(providers, Provider{
			ID:      strconv.Itoa(entry.ID),
			Name:    entry.Provider,
			APIURL:  entry.Endpoint,
			Keys:    entry.Keys,
			Adapter: entry.Adapter,
		})
	}
	return providers, nil
}

// Add writes a provider and assigns its ID. The ID is only a label for the API;
// the name is what providers are looked up by.
func (s *Store) Add(provider Provider) error {
	file, err := s.read()
	if err != nil {
		return err
	}
	id := file.NextID
	if id < 1 {
		id = 1
	}
	file.Providers = append(file.Providers, providerEntry{
		ID:       id,
		Provider: provider.Name,
		Endpoint: provider.APIURL,
		Keys:     nonNilKeys(provider.Keys),
		Adapter:  provider.Adapter,
	})
	file.NextID = id + 1
	return s.write(file)
}

func (s *Store) Remove(name string) error {
	file, err := s.read()
	if err != nil {
		return err
	}
	kept := file.Providers[:0]
	for _, entry := range file.Providers {
		if !strings.EqualFold(entry.Provider, name) {
			kept = append(kept, entry)
		}
	}
	file.Providers = kept
	return s.write(file)
}

func (s *Store) SetKeys(name string, keys []string) error {
	file, err := s.read()
	if err != nil {
		return err
	}
	for i := range file.Providers {
		if strings.EqualFold(file.Providers[i].Provider, name) {
			file.Providers[i].Keys = nonNilKeys(keys)
		}
	}
	return s.write(file)
}

// AddProvider writes a provider for the API. The caller reloads the list
// afterwards and reads the new provider back, so the ID is never guessed here.
func AddProvider(name, endpoint, adapter string, keys []string) error {
	if IsBuiltin(name) {
		return fmt.Errorf("config: %s is a built-in provider and cannot be added", name)
	}
	s, err := current()
	if err != nil {
		return err
	}
	return s.Add(Provider{Name: name, APIURL: strings.TrimSpace(endpoint), Adapter: adapter, Keys: keys})
}

func RemoveProvider(name string) error {
	if IsBuiltin(name) {
		return fmt.Errorf("config: %s is a built-in provider and cannot be removed", name)
	}
	s, err := current()
	if err != nil {
		return err
	}
	return s.Remove(name)
}

func SetProviderKeys(name string, keys []string) error {
	if IsBuiltin(name) {
		return fmt.Errorf("config: %s is a built-in provider and cannot be changed", name)
	}
	s, err := current()
	if err != nil {
		return err
	}
	return s.SetKeys(name, keys)
}

// nonNilKeys keeps a provider with no key as an empty list rather than null, so
// the stored JSON always has an array a reader can iterate.
func nonNilKeys(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

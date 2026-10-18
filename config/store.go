package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Store keeps the providers in the engine's own SQLite file. An API key is the
// one thing here that cannot be recreated from anywhere else, which is why the
// providers hold a table of their own rather than a settings blob.
type Store struct {
	handle *sql.DB
}

func NewStore(handle *sql.DB) *Store {
	return &Store{handle: handle}
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

// List returns the providers in the order the store holds them, which is the
// order the phone sees.
func (s *Store) List() ([]Provider, error) {
	rows, err := s.handle.Query(`SELECT idx, provider, endpoint, keys, adapter FROM llm_providers ORDER BY idx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	providers := make([]Provider, 0)
	for rows.Next() {
		var provider Provider
		var id int64
		var keys string
		if err := rows.Scan(&id, &provider.Name, &provider.APIURL, &keys, &provider.Adapter); err != nil {
			return nil, err
		}
		provider.ID = strconv.FormatInt(id, 10)
		provider.Keys = parseKeys(keys)
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

func (s *Store) Count() (int, error) {
	var count int
	err := s.handle.QueryRow(`SELECT COUNT(*) FROM llm_providers`).Scan(&count)
	return count, err
}

// Add writes a provider and lets SQLite assign its ID.
func (s *Store) Add(provider Provider) error {
	_, err := s.handle.Exec(`INSERT INTO llm_providers (provider, endpoint, keys, adapter) VALUES (?,?,?,?)`,
		provider.Name, provider.APIURL, encodeKeys(provider.Keys), provider.Adapter)
	return err
}

// Import writes providers with the IDs they already carry, in one transaction:
// a half-imported store would hold providers, pass the "is it empty" gate and
// never be retried.
func (s *Store) Import(providers []Provider) error {
	tx, err := s.handle.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, provider := range providers {
		if _, err := tx.Exec(`INSERT INTO llm_providers (idx, provider, endpoint, keys, adapter) VALUES (?,?,?,?,?)`,
			provider.ID, provider.Name, provider.APIURL, encodeKeys(provider.Keys), provider.Adapter); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Remove(name string) error {
	_, err := s.handle.Exec(`DELETE FROM llm_providers WHERE provider=?`, name)
	return err
}

func (s *Store) SetKeys(name string, keys []string) error {
	_, err := s.handle.Exec(`UPDATE llm_providers SET keys=? WHERE provider=?`, encodeKeys(keys), name)
	return err
}

// AddProvider writes a provider for the API. The caller reloads the list
// afterwards and reads the new provider back, so the ID is never guessed here.
func AddProvider(name, endpoint, adapter string, keys []string) error {
	s, err := current()
	if err != nil {
		return err
	}
	return s.Add(Provider{Name: name, APIURL: strings.TrimSpace(endpoint), Adapter: adapter, Keys: keys})
}

func RemoveProvider(name string) error {
	s, err := current()
	if err != nil {
		return err
	}
	return s.Remove(name)
}

func SetProviderKeys(name string, keys []string) error {
	s, err := current()
	if err != nil {
		return err
	}
	return s.SetKeys(name, keys)
}

// encodeKeys stores the key list as JSON: order is the rotation order, and a
// provider with no key is stored as an empty list rather than NULL.
func encodeKeys(keys []string) string {
	if keys == nil {
		keys = []string{}
	}
	encoded, _ := json.Marshal(keys)
	return string(encoded)
}

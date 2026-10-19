package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// QueryFunc runs one SQL statement against D1. It is db.Query in production and
// a fake in tests, so the store can be exercised without a network.
type QueryFunc func(sql string, params ...any) ([]map[string]any, error)

// Store keeps the providers in D1. An API key is the one thing here that cannot
// be recreated from anywhere else, so it lives in a table of its own rather
// than in a settings blob - and in D1 rather than on this machine, so a fresh
// session still finds it.
type Store struct {
	query QueryFunc
}

func NewStore(query QueryFunc) *Store {
	return &Store{query: query}
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

// Ensure creates the provider table if it is missing. D1 dropped it in the
// local-store period, so the first boot after that has to put it back before
// anything can be read or written.
func (s *Store) Ensure() error {
	return write(s.query, `CREATE TABLE IF NOT EXISTS llm_providers (
		idx INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL UNIQUE,
		endpoint TEXT NOT NULL DEFAULT '',
		keys TEXT NOT NULL DEFAULT '[]',
		adapter TEXT NOT NULL DEFAULT ''
	)`)
}

// List returns the providers in the order D1 holds them, which is the order the
// phone sees. D1's column is called `index`, which SQLite only accepts quoted.
func (s *Store) List() ([]Provider, error) {
	rows, err := s.query(`SELECT "index" AS idx, provider, endpoint, keys, adapter FROM llm_providers ORDER BY "index"`)
	if err != nil {
		return nil, err
	}

	providers := make([]Provider, 0, len(rows))
	for _, row := range rows {
		providers = append(providers, Provider{
			ID:      intString(row["idx"]),
			Name:    stringValue(row["provider"]),
			APIURL:  stringValue(row["endpoint"]),
			Keys:    parseKeys(row["keys"]),
			Adapter: stringValue(row["adapter"]),
		})
	}
	return providers, nil
}

func (s *Store) Count() (int, error) {
	rows, err := s.query(`SELECT COUNT(*) AS n FROM llm_providers`)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	switch value := rows[0]["n"].(type) {
	case float64:
		return int(value), nil
	case int:
		return value, nil
	case int64:
		return int(value), nil
	default:
		return 0, fmt.Errorf("config: unexpected count %v", rows[0]["n"])
	}
}

// Add writes a provider and lets D1 assign its ID.
func (s *Store) Add(provider Provider) error {
	return write(s.query, `INSERT INTO llm_providers (provider, endpoint, keys, adapter) VALUES (?,?,?,?)`,
		provider.Name, provider.APIURL, encodeKeys(provider.Keys), provider.Adapter)
}

func (s *Store) Remove(name string) error {
	return write(s.query, `DELETE FROM llm_providers WHERE provider=?`, name)
}

func (s *Store) SetKeys(name string, keys []string) error {
	return write(s.query, `UPDATE llm_providers SET keys=? WHERE provider=?`, encodeKeys(keys), name)
}

// EnsureStore creates the provider table through the store set at boot.
func EnsureStore() error {
	s, err := current()
	if err != nil {
		return err
	}
	return s.Ensure()
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

// write runs a statement whose rows nobody reads. D1 reports failures in the
// response body rather than the status, and db.Query turns those into an error,
// so success is simply the call not failing.
func write(query QueryFunc, sql string, params ...any) error {
	_, err := query(sql, params...)
	return err
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

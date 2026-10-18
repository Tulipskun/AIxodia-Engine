package config

import (
	"fmt"
	"log"
	"strings"

	"aixodia-engine/localdb"
)

// QueryFunc is how the import reads D1: db.Query, passed in so the read is
// visible at the call site and testable without a network.
type QueryFunc func(sql string, params ...any) ([]map[string]any, error)

const (
	// D1's column is called `index`, which SQLite only accepts quoted.
	legacyProvidersSQL = `SELECT "index", provider, endpoint, keys, adapter FROM llm_providers`
	// legacyImportStateKey records that the one-time copy is over. Without it,
	// "the local store has no provider" would read as "the import never ran",
	// and a phone that removed its last provider would be handed them back from
	// D1 on the next tick — which is not what removing a provider means.
	legacyImportStateKey = "providers/legacy-import"
)

// ImportLegacyProviders moves the providers — and with them the API keys — from
// D1 into the local store, once. It cannot overwrite a provider added or edited
// here, and it does nothing at all after the first resolution. The one case it
// retries is a read that failed: D1 still holds the keys and the phone has
// none, so the next boot has to try again.
func ImportLegacyProviders(query QueryFunc) (int, error) {
	store, err := current()
	if err != nil {
		return 0, err
	}
	done, err := legacyImportDone(store)
	if err != nil {
		return 0, err
	}
	if done {
		return 0, nil
	}
	count, err := store.Count()
	if err != nil {
		return 0, fmt.Errorf("config: count providers: %w", err)
	}
	if count > 0 {
		// Providers are already here, so there is nothing to carry over — and
		// carrying them over later could only resurrect removed ones.
		return 0, markLegacyImportDone(store)
	}

	rows, err := query(legacyProvidersSQL)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			// D1's table is gone: expected once it is dropped, and no reason to
			// ask again on every tick.
			return 0, markLegacyImportDone(store)
		}
		return 0, fmt.Errorf("config: read D1 providers: %w", err)
	}

	providers := make([]Provider, 0, len(rows))
	for _, row := range rows {
		providers = append(providers, Provider{
			ID:      intString(row["index"]),
			Name:    stringValue(row["provider"]),
			APIURL:  stringValue(row["endpoint"]),
			Keys:    parseKeys(row["keys"]),
			Adapter: stringValue(row["adapter"]),
		})
	}
	if len(providers) == 0 {
		// D1's table exists and holds nothing: there is nothing to copy.
		return 0, markLegacyImportDone(store)
	}
	if err := store.Import(providers); err != nil {
		return 0, fmt.Errorf("config: import providers: %w", err)
	}
	log.Printf("config: imported %d providers from D1", len(providers))
	return len(providers), markLegacyImportDone(store)
}

func legacyImportDone(store *Store) (bool, error) {
	_, found, err := localdb.GetState(store.handle, legacyImportStateKey)
	if err != nil {
		return false, fmt.Errorf("config: read %s: %w", legacyImportStateKey, err)
	}
	return found, nil
}

func markLegacyImportDone(store *Store) error {
	return localdb.PutState(store.handle, legacyImportStateKey, "1")
}

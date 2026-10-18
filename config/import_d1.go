package config

import (
	"fmt"
	"log"
	"strings"
)

// QueryFunc is how the import reads D1: db.Query, passed in so the read is
// visible at the call site and testable without a network.
type QueryFunc func(sql string, params ...any) ([]map[string]any, error)

// D1's column is called `index`, which SQLite only accepts quoted.
const legacyProvidersSQL = `SELECT "index", provider, endpoint, keys, adapter FROM llm_providers`

// ImportLegacyProviders moves the providers — and with them the API keys — from
// D1 into the local store. It runs only while the local store holds none, so it
// cannot overwrite a provider added or edited here, and a boot whose read fails
// simply tries again on the next one. Once D1's llm_providers is dropped the
// read finds no table, which is the expected end state and not an error.
func ImportLegacyProviders(query QueryFunc) (int, error) {
	store, err := current()
	if err != nil {
		return 0, err
	}
	count, err := store.Count()
	if err != nil {
		return 0, fmt.Errorf("config: count providers: %w", err)
	}
	if count > 0 {
		return 0, nil
	}

	rows, err := query(legacyProvidersSQL)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
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
		return 0, nil
	}
	if err := store.Import(providers); err != nil {
		return 0, fmt.Errorf("config: import providers: %w", err)
	}
	log.Printf("config: imported %d providers from D1", len(providers))
	return len(providers), nil
}

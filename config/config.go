package config

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

type Provider struct {
	ID      string
	Name    string
	APIURL  string
	Keys    []string
	Adapter string
}

// builtinProviders are the providers the engine carries itself, so the phone
// has something to talk to before - or without - any provider being added. The
// opencode free tier needs no API key, which is what makes it safe to ship
// turned on and why it is not stored in D1.
var builtinProviders = []Provider{
	{
		Name:    "Opencode",
		APIURL:  "https://opencode.ai/inference/v1",
		Adapter: "opencode",
	},
}

var (
	Providers []Provider
	mu        sync.RWMutex
)

// LoadConfig reads the providers into memory. They live in D1, so this is the
// one place the engine reads them; the built-ins are carried by the engine and
// appended after them, so a D1 row with the same name wins the lookup.
func LoadConfig() error {
	store, err := current()
	if err != nil {
		return err
	}
	providers, err := store.List()
	if err != nil {
		// The built-ins do not depend on D1. Publish them even when the read
		// fails so a boot without D1 still has the free provider, and keep the
		// last good list instead of dropping it.
		mu.Lock()
		if len(Providers) == 0 {
			Providers = append([]Provider(nil), builtinProviders...)
		}
		mu.Unlock()
		return fmt.Errorf("config: load providers: %w", err)
	}
	providers = append(providers, builtinProviders...)

	mu.Lock()
	Providers = providers
	mu.Unlock()

	log.Printf("config: loaded %d providers (%d built-in)", len(providers), len(builtinProviders))
	return nil
}

// IsBuiltin reports whether name belongs to a provider the engine carries
// itself. Built-ins are always listed and can never be added, edited or
// removed through the API.
func IsBuiltin(name string) bool {
	for _, provider := range builtinProviders {
		if strings.EqualFold(provider.Name, name) {
			return true
		}
	}
	return false
}

func GetProviders() []Provider {
	mu.RLock()
	defer mu.RUnlock()

	result := make([]Provider, len(Providers))
	copy(result, Providers)
	return result
}

func GetProvider(id string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()

	for _, provider := range Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

func GetProviderByName(name string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()

	for _, provider := range Providers {
		if strings.EqualFold(provider.Name, name) {
			return provider, true
		}
	}
	return Provider{}, false
}

func intString(value any) string {
	switch value := value.(type) {
	case float64:
		return fmt.Sprint(int(value))
	case int:
		return fmt.Sprint(value)
	default:
		return stringValue(value)
	}
}

func parseKeys(value any) []string {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

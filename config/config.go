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

var (
	Providers []Provider
	mu        sync.RWMutex
)

// LoadConfig reads the providers into memory. They live in the engine's own
// store now, so this is a file read: nothing the phone does waits on D1.
func LoadConfig() error {
	store, err := current()
	if err != nil {
		return err
	}
	providers, err := store.List()
	if err != nil {
		return fmt.Errorf("config: load providers: %w", err)
	}

	mu.Lock()
	Providers = providers
	mu.Unlock()

	log.Printf("config: loaded %d providers", len(providers))
	return nil
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

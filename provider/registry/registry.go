package registry

import (
	"fmt"
	"sort"

	"aixodia-engine/provider"
	"aixodia-engine/provider/anthropic"
	"aixodia-engine/provider/openai"
	"aixodia-engine/provider/opencode"
)

type Registry struct {
	adapters map[string]provider.Adapter
}

func New() *Registry {
	adapter := openai.New()
	return &Registry{adapters: map[string]provider.Adapter{
		"openai": adapter,
		// Gemini exposes an OpenAI-compatible endpoint, so the openai adapter
		// serves it with the provider's endpoint set to that base URL.
		"gemini":    adapter,
		"anthropic": anthropic.New(),
		"opencode":  opencode.New(),
		// The free tier refuses every client but the real one, so this adapter
		// runs that client instead of reimplementing its request. See the
		// package comment for what was measured.
	}}
}

func (r *Registry) Get(name string) (provider.Adapter, bool) {
	adapter, ok := r.adapters[name]
	return adapter, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Require(name string) (provider.Adapter, error) {
	adapter, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q (available: %v)", name, r.Names())
	}
	return adapter, nil
}

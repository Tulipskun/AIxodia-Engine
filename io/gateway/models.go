package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-engine/config"
	"ai-engine/provider"
)

const modelCacheTTL = time.Minute

type modelEntry struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	only := strings.TrimSpace(r.URL.Query().Get("provider"))
	providers := config.GetProviders()

	filtered := make([]config.Provider, 0, len(providers))
	for _, provider := range providers {
		if only != "" && !strings.EqualFold(provider.Name, only) {
			continue
		}
		filtered = append(filtered, provider)
	}
	if only != "" && len(filtered) == 0 {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown provider %q", only))
		return
	}

	var (
		wait   sync.WaitGroup
		lock   sync.Mutex
		models []modelEntry
		failed = map[string]string{}
	)

	for _, cfg := range filtered {
		wait.Add(1)
		go func(cfg config.Provider) {
			defer wait.Done()
			ids, err := g.providerModels(cfg)
			lock.Lock()
			defer lock.Unlock()
			if err != nil {
				failed[cfg.Name] = err.Error()
				return
			}
			for _, id := range ids {
				models = append(models, modelEntry{ID: id, Provider: cfg.Name})
			}
		}(cfg)
	}
	wait.Wait()

	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider == models[j].Provider {
			return models[i].ID < models[j].ID
		}
		return models[i].Provider < models[j].Provider
	})

	payload := map[string]any{"models": models}
	if len(failed) > 0 {
		payload["errors"] = failed
	}
	writeJSON(w, http.StatusOK, payload)
}

func (g *Gateway) providerModels(cfg config.Provider) ([]string, error) {
	g.modelMu.Lock()
	if cached, ok := g.modelList[cfg.Name]; ok && time.Since(cached.fetched) < modelCacheTTL {
		g.modelMu.Unlock()
		return cached.ids, nil
	}
	g.modelMu.Unlock()

	ids, err := fetchModels(cfg)
	if err != nil {
		return nil, err
	}

	g.modelMu.Lock()
	g.modelList[cfg.Name] = modelCache{fetched: time.Now(), ids: ids}
	g.modelMu.Unlock()
	return ids, nil
}

func fetchModels(cfg config.Provider) ([]string, error) {
	// Non-login mode: opencode free tier needs no key. Other providers still do.
	if len(cfg.Keys) == 0 && cfg.Adapter != "opencode" {
		return nil, fmt.Errorf("provider has no API keys")
	}
	endpoint, err := provider.Endpoint(cfg.APIURL, "/v1/models", "/models")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if len(cfg.Keys) > 0 {
		request.Header.Set("Authorization", "Bearer "+cfg.Keys[0])
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", provider.UserAgent)

	response, err := provider.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &provider.APIError{StatusCode: response.StatusCode, Message: provider.ErrorMessage(body)}
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("provider returned an unexpected /models payload")
	}

	ids := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

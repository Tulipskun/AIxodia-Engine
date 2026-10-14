package gateway

// The /api routes are what the AIxodia app's Settings screen and per-chat agent
// settings call over the tunnel. Providers, keys and agent routes live in D1
// (providers, sessions, state tables). The probe results are kept in memory and
// reset when the daemon restarts.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"ai-engine/config"
	"ai-engine/db"
	"ai-engine/provider/registry"
)

var providerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type probeResult struct {
	Probed       bool
	Reachable    bool
	ModelCount   int
	WorkingModel string
	LastError    string
	Models       []string
}

type probeCache struct {
	mu      sync.Mutex
	results map[string]probeResult
}

func newProbeCache() *probeCache {
	return &probeCache{results: map[string]probeResult{}}
}

func (c *probeCache) get(name string) probeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.results[name]
}

func (c *probeCache) set(name string, result probeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[name] = result
}

func (c *probeCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.results, name)
}

// applyKeyChanges returns the new key list. replace wins over everything; then
// remove (by index into the current list), then add.
func applyKeyChanges(current, add []string, remove []int, replace []string) []string {
	if len(replace) > 0 {
		return cleanKeys(replace)
	}
	keys := append([]string(nil), current...)
	indexes := append([]int(nil), remove...)
	sort.Sort(sort.Reverse(sort.IntSlice(indexes)))
	for _, index := range indexes {
		if index >= 0 && index < len(keys) {
			keys = append(keys[:index], keys[index+1:]...)
		}
	}
	return cleanKeys(append(keys, add...))
}

func cleanKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func providerStatusJSON(cfg config.Provider, probe probeResult) map[string]any {
	return map[string]any{
		"id":            cfg.Name,
		"adapter":       cfg.Adapter,
		"endpoint":      cfg.APIURL,
		"free_only":     cfg.Free,
		"key_count":     len(cfg.Keys),
		"model_count":   probe.ModelCount,
		"reachable":     probe.Reachable,
		"probed":        probe.Probed,
		"working_model": probe.WorkingModel,
		"last_error":    probe.LastError,
	}
}

// RequireToken guards the whole API with the same bearer token the phone uses
// for its WebSocket. /healthz stays open so the tunnel can be checked.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "token rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/providers", g.apiListProviders)
	mux.HandleFunc("POST /api/providers", g.apiAddProvider)
	mux.HandleFunc("POST /api/providers/refresh", g.apiRefreshProviders)
	mux.HandleFunc("POST /api/providers/{id}/refresh", g.apiRefreshProvider)
	mux.HandleFunc("POST /api/providers/{id}/keys", g.apiChangeKeys)
	mux.HandleFunc("DELETE /api/providers/{id}", g.apiRemoveProvider)
	mux.HandleFunc("GET /api/models", g.apiModels)
	mux.HandleFunc("GET /api/settings", g.apiGetSettings)
	mux.HandleFunc("PUT /api/settings", g.apiPutSettings)
	mux.HandleFunc("POST /v1/attachments", g.apiUploadAttachment)
	mux.HandleFunc("POST /api/engine/update", g.apiEngineUpdate)
	mux.HandleFunc("GET /api/ui", g.apiGetUI)
	mux.HandleFunc("PUT /api/ui", g.apiPutUI)
	mux.HandleFunc("GET /api/sessions/{id}", g.apiGetSessionAgent)
	mux.HandleFunc("PATCH /api/sessions/{id}", g.apiPatchSessionAgent)
}

func (g *Gateway) apiListProviders(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, cfg := range config.GetProviders() {
		list = append(list, providerStatusJSON(cfg, g.probes.get(cfg.Name)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

type addProviderRequest struct {
	ID       string   `json:"id"`
	Adapter  string   `json:"adapter"`
	Endpoint string   `json:"endpoint"`
	Keys     []string `json:"keys"`
	FreeOnly bool     `json:"free_only"`
}

func (g *Gateway) apiAddProvider(w http.ResponseWriter, r *http.Request) {
	var body addProviderRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if !providerIDPattern.MatchString(body.ID) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "provider id must be letters, digits, '.', '_' or '-'")
		return
	}
	if _, exists := config.GetProviderByName(body.ID); exists {
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf("provider %q already exists", body.ID))
		return
	}
	if _, err := registry.New().Require(body.Adapter); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if strings.TrimSpace(body.Endpoint) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "endpoint is required")
		return
	}
	keys := cleanKeys(body.Keys)
	keysJSON, _ := json.Marshal(keys)
	if _, err := db.Insert("providers", map[string]any{
		"provider": body.ID,
		"endpoint": strings.TrimSpace(body.Endpoint),
		"keys":     string(keysJSON),
		"adapter":  body.Adapter,
		"free":     boolInt(body.FreeOnly),
	}); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	g.reloadProviders()
	cfg, _ := config.GetProviderByName(body.ID)
	writeJSON(w, http.StatusCreated, providerStatusJSON(cfg, probeResult{}))
}

func (g *Gateway) apiRemoveProvider(w http.ResponseWriter, r *http.Request) {
	cfg, ok := config.GetProviderByName(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider")
		return
	}
	if err := db.Delete("providers", db.Where{"provider": cfg.Name}); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	g.probes.forget(cfg.Name)
	g.reloadProviders()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type changeKeysRequest struct {
	Add     []string `json:"add"`
	Remove  []int    `json:"remove"`
	Replace []string `json:"replace"`
}

func (g *Gateway) apiChangeKeys(w http.ResponseWriter, r *http.Request) {
	cfg, ok := config.GetProviderByName(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider")
		return
	}
	var body changeKeysRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	keys := applyKeyChanges(cfg.Keys, body.Add, body.Remove, body.Replace)
	keysJSON, _ := json.Marshal(keys)
	if _, err := db.Update("providers", map[string]any{"keys": string(keysJSON)}, db.Where{"provider": cfg.Name}); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	g.probes.forget(cfg.Name)
	g.reloadProviders()
	updated, _ := config.GetProviderByName(cfg.Name)
	writeJSON(w, http.StatusOK, providerStatusJSON(updated, probeResult{}))
}

// probeLimit caps how many upstreams are asked at once. With a hundred
// providers a sequential refresh would wait on each timeout in turn.
const probeLimit = 8

// forEachBounded runs fn for every item with at most limit calls in flight.
func forEachBounded[T any](items []T, limit int, fn func(int, T)) {
	var wait sync.WaitGroup
	slots := make(chan struct{}, limit)
	for i, item := range items {
		wait.Add(1)
		slots <- struct{}{}
		go func(i int, item T) {
			defer wait.Done()
			defer func() { <-slots }()
			fn(i, item)
		}(i, item)
	}
	wait.Wait()
}

func (g *Gateway) apiRefreshProviders(w http.ResponseWriter, r *http.Request) {
	providers := config.GetProviders()
	list := make([]map[string]any, len(providers))
	forEachBounded(providers, probeLimit, func(i int, cfg config.Provider) {
		list[i] = providerStatusJSON(cfg, g.probeProvider(cfg))
	})
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

func (g *Gateway) apiRefreshProvider(w http.ResponseWriter, r *http.Request) {
	cfg, ok := config.GetProviderByName(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider")
		return
	}
	writeJSON(w, http.StatusOK, providerStatusJSON(cfg, g.probeProvider(cfg)))
}

// probeProvider asks the upstream for its model list, which doubles as a check
// that the endpoint and the first key work.
func (g *Gateway) probeProvider(cfg config.Provider) probeResult {
	result := probeResult{Probed: true}
	models, err := fetchModels(cfg)
	if err != nil {
		result.LastError = err.Error()
	} else {
		result.Reachable = true
		result.Models = models
		result.ModelCount = len(models)
		if len(models) > 0 {
			result.WorkingModel = models[0]
		}
	}
	g.probes.set(cfg.Name, result)
	return result
}

func (g *Gateway) apiModels(w http.ResponseWriter, r *http.Request) {
	type modelView struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		SupportsStreaming bool   `json:"supports_streaming"`
		SupportsTools     bool   `json:"supports_tools"`
	}
	type providerView struct {
		ID           string      `json:"id"`
		Name         string      `json:"name"`
		DefaultModel string      `json:"default_model"`
		Models       []modelView `json:"models"`
	}

	providers := config.GetProviders()
	savedProvider, savedModel := g.savedMainModel()
	views := make([]providerView, len(providers))
	// providerModels caches each upstream's list for a minute, so opening the
	// model picker does not ask all hundred providers again.
	forEachBounded(providers, probeLimit, func(i int, cfg config.Provider) {
		view := providerView{ID: cfg.Name, Name: cfg.Name, Models: []modelView{}}
		ids, err := g.providerModels(cfg)
		if err == nil {
			for _, id := range ids {
				view.Models = append(view.Models, modelView{
					ID:                id,
					Name:              id,
					SupportsStreaming: true,
					SupportsTools:     cfg.Adapter == "openai" || cfg.Adapter == "opencode" || cfg.Adapter == "anthropic",
				})
			}
			if len(ids) > 0 {
				view.DefaultModel = ids[0]
				// Point the default at the model saved in settings when it
				// belongs to this provider, so the picker opens on what the
				// user actually chose instead of the upstream list head.
				if savedModel != "" && cfg.Name == savedProvider {
					for _, id := range ids {
						if id == savedModel {
							view.DefaultModel = id
							break
						}
					}
				}
			}
		}
		views[i] = view
	})
	writeJSON(w, http.StatusOK, map[string]any{"providers": views})
}

const settingsStateKey = "settings"

// savedMainModel returns the provider/model saved in settings, or ""/""
// when nothing usable is stored.
func (g *Gateway) savedMainModel() (string, string) {
	rows, err := db.Select("state", db.Where{"key": settingsStateKey})
	if err != nil || len(rows) == 0 {
		return "", ""
	}
	var saved struct {
		Main struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"main"`
	}
	if err := json.Unmarshal([]byte(rowString(rows[0], "value")), &saved); err != nil {
		return "", ""
	}
	return saved.Main.Provider, saved.Main.Model
}

func defaultSettings() json.RawMessage {
	return json.RawMessage(`{"main":{"provider":"","model":"","generation":{}},"sub":{"provider":"","model":"","generation":{}},"sub_enabled":true}`)
}

func (g *Gateway) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Select("state", db.Where{"key": settingsStateKey})
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	if len(rows) == 0 {
		writeRaw(w, defaultSettings())
		return
	}
	stored := rowString(rows[0], "value")
	if !json.Valid([]byte(stored)) || stored == "" {
		writeRaw(w, defaultSettings())
		return
	}
	writeRaw(w, json.RawMessage(stored))
}

func (g *Gateway) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := decodeBody(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "settings must be a JSON object")
		return
	}
	if _, err := db.Query(
		"INSERT INTO state (key, value, updated_at) VALUES (?, ?, unixepoch()) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		settingsStateKey, string(raw),
	); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeRaw(w, raw)
}

func (g *Gateway) apiGetSessionAgent(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Select("sessions", db.Where{"id": r.PathValue("id")})
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	row := rows[0]
	model := rowString(row, "model")
	subModel := rowString(row, "sub_model")
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":     rowString(row, "provider"),
		"model":        model,
		"pinned":       model != "",
		"sub_provider": rowString(row, "sub_provider"),
		"sub_model":    subModel,
		"sub_enabled":  rowBool(row, "sub_enabled"),
		"sub_pinned":   subModel != "",
	})
}

type patchSessionAgentRequest struct {
	Provider    *string `json:"provider"`
	Model       *string `json:"model"`
	ClearModel  bool    `json:"clear_model"`
	SubProvider *string `json:"sub_provider"`
	SubModel    *string `json:"sub_model"`
	ClearSub    bool    `json:"clear_sub"`
	SubEnabled  *bool   `json:"sub_enabled"`
}

// applySessionAgentPatch turns a PATCH body into the column values to write.
func applySessionAgentPatch(body patchSessionAgentRequest) map[string]any {
	data := map[string]any{}
	if body.ClearModel {
		data["provider"] = ""
		data["model"] = ""
	}
	if body.Provider != nil {
		data["provider"] = strings.TrimSpace(*body.Provider)
	}
	if body.Model != nil {
		data["model"] = strings.TrimSpace(*body.Model)
	}
	if body.ClearSub {
		data["sub_provider"] = ""
		data["sub_model"] = ""
	}
	if body.SubProvider != nil {
		data["sub_provider"] = strings.TrimSpace(*body.SubProvider)
	}
	if body.SubModel != nil {
		data["sub_model"] = strings.TrimSpace(*body.SubModel)
	}
	if body.SubEnabled != nil {
		data["sub_enabled"] = boolInt(*body.SubEnabled)
	}
	return data
}

func (g *Gateway) apiPatchSessionAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body patchSessionAgentRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	data := applySessionAgentPatch(body)
	if len(data) > 0 {
		if _, err := db.Update("sessions", data, db.Where{"id": id}); err != nil {
			writeError(w, http.StatusBadGateway, "database_error", err.Error())
			return
		}
	}
	g.apiGetSessionAgent(w, r)
}

func (g *Gateway) reloadProviders() {
	if err := config.LoadConfig(); err != nil {
		log.Printf("gateway: reload providers: %v", err)
	}
}

func writeRaw(w http.ResponseWriter, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func rowString(row map[string]any, key string) string {
	switch value := row[key].(type) {
	case string:
		return value
	case float64:
		return fmt.Sprint(int64(value))
	default:
		return ""
	}
}

func rowBool(row map[string]any, key string) bool {
	switch value := row[key].(type) {
	case float64:
		return value != 0
	case bool:
		return value
	case string:
		return value == "1" || value == "true"
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

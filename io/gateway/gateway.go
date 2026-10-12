package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-engine/config"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
)

const (
	maxToolRounds  = 4
	keyAttempts    = 3
	upstreamWait   = 180 * time.Second
	requestMaxBody = 8 << 20
)

// Version is the release tag, baked in at build time. Bump on every release
// so a live service can be identified via GET /healthz.
const Version = "v1.119"

type SessionStore interface {
	Create(params session.NewSession) (*session.Session, error)
	Get(id string) (*session.Session, error)
	List() ([]session.Session, error)
	Delete(id string) error
	SaveConfig(id string, saved session.Session) error
	Turns(id string) ([]session.Turn, error)
	History(id string) ([]provider.Message, error)
	AppendTurns(id string, turns []session.TurnInput) error
}

type Gateway struct {
	sessions  SessionStore
	registry  *registry.Registry
	tools     map[string]tools.Tool
	mu        sync.Mutex
	keyIndex  map[string]int
	modelMu   sync.Mutex
	modelList map[string]modelCache
	brMu      sync.Mutex
	breakers  map[string]*breaker
	probes    *probeCache
}

type modelCache struct {
	fetched time.Time
	ids     []string
}

func New(sessions SessionStore, reg *registry.Registry, toolset map[string]tools.Tool) *Gateway {
	return &Gateway{
		sessions:  sessions,
		registry:  reg,
		tools:     toolset,
		keyIndex:  map[string]int{},
		modelList: map[string]modelCache{},
		breakers:  map[string]*breaker{},
		probes:    newProbeCache(),
	}
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
	})
	mux.HandleFunc("GET /v1/providers", g.listProviders)
	mux.HandleFunc("GET /v1/models", g.listModels)
	mux.HandleFunc("POST /v1/sessions", g.createSession)
	mux.HandleFunc("GET /v1/sessions", g.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", g.getSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", g.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/turns", g.getSessionTurns)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	g.registerAPI(mux)
	return mux
}

func (g *Gateway) listProviders(w http.ResponseWriter, r *http.Request) {
	providers := config.GetProviders()
	list := make([]map[string]any, 0, len(providers))
	for _, provider := range providers {
		list = append(list, map[string]any{
			"name":     provider.Name,
			"adapter":  provider.Adapter,
			"endpoint": provider.APIURL,
			"free":     provider.Free,
			"keys":     len(provider.Keys),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

type createSessionRequest struct {
	Title       string   `json:"title"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	SubProvider string   `json:"sub_provider"`
	SubModel    string   `json:"sub_model"`
	SubEnabled  bool     `json:"sub_enabled"`
	Reasoning   bool     `json:"reasoning"`
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	MaxTokens   int      `json:"max_tokens"`
}

func (g *Gateway) createSession(w http.ResponseWriter, r *http.Request) {
	var body createSessionRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if body.Provider != "" {
		if _, ok := config.GetProviderByName(body.Provider); !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("unknown provider %q", body.Provider))
			return
		}
	}
	created, err := g.sessions.Create(session.NewSession{
		Title:       body.Title,
		Provider:    body.Provider,
		Model:       body.Model,
		SubProvider: body.SubProvider,
		SubModel:    body.SubModel,
		SubEnabled:  body.SubEnabled,
		Reasoning:   body.Reasoning,
		Temperature: body.Temperature,
		TopP:        body.TopP,
		MaxTokens:   body.MaxTokens,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (g *Gateway) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := g.sessions.List()
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (g *Gateway) getSession(w http.ResponseWriter, r *http.Request) {
	found, err := g.sessions.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (g *Gateway) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := g.sessions.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (g *Gateway) getSessionTurns(w http.ResponseWriter, r *http.Request) {
	turns, err := g.sessions.Turns(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns})
}

func (g *Gateway) nextKey(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	index := g.keyIndex[name]
	g.keyIndex[name] = index + 1
	return index
}

func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, requestMaxBody+1))
	if err != nil {
		return fmt.Errorf("cannot read body: %w", err)
	}
	if len(raw) > requestMaxBody {
		return fmt.Errorf("request body exceeds the %d-byte limit", requestMaxBody)
	}
	// encoding/json silently replaces invalid bytes with U+FFFD, so a Thai
	// message sent in the wrong encoding would be saved as a row of "�".
	// Refuse it instead so the sender can see the problem.
	if !utf8.Valid(raw) {
		return fmt.Errorf("request body is not valid UTF-8; send the message as UTF-8 text")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("cannot decode JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
		},
	})
}

func newCompletionID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "..."
}

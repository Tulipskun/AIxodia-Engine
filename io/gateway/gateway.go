package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aixodia-engine/config"
	"aixodia-engine/goals"
	"aixodia-engine/provider"
	"aixodia-engine/provider/registry"
	"aixodia-engine/session"
	"aixodia-engine/tools"
)

const (
	// maxToolRounds is how many model→tool→model rounds one turn may spend.
	// Four was not enough for real work: a session asking for a change to a
	// notebook used read, bash, read, bash and was cut off there, so the turn
	// ended with the model still narrating what it was about to look at and
	// nothing done (D1 turns for session smv6psjidc792, seq 15-26).
	maxToolRounds  = 100
	keyAttempts    = 3
	upstreamWait   = 180 * time.Second
	requestMaxBody = 8 << 20
)

// sessionIDPattern is what a client-supplied draft id may contain. The phone
// puts this id in URLs and on disk, so path separators and whitespace must not
// get through.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Version is the release tag, baked in at build time. Bump on every release
// so a live service can be identified via GET /healthz.
const Version = "v1.153"

type SessionStore interface {
	Create(params session.NewSession) (*session.Session, error)
	Get(id string) (*session.Session, error)
	List() ([]session.Session, error)
	Delete(id string) error
	SaveConfig(id string, saved session.Session) error
	SaveAgent(id string, patch session.AgentPatch) error
	Turns(id string) ([]session.Turn, error)
	History(id string) ([]provider.Message, error)
	AppendTurns(id string, turns []session.TurnInput) ([]session.Turn, error)
	Unfinished(limit int) ([]session.UnfinishedTurn, error)
}

// PagedTurnStore is the optional half of SessionStore that can read one page
// of a transcript. It is kept separate so stores that only implement Turns
// (tests, older adapters) still work: the route slices their full answer.
type PagedTurnStore interface {
	TurnsPage(id string, beforeSeq int64, limit int) ([]session.Turn, error)
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
	logReader func(int, string) []map[string]any
	probes    *probeCache
	startedAt int64
	// uploadDir is where attachments are stored, so the agent can read them.
	uploadDir string
	// state is where the phone's settings live: D1's system_state in
	// production. Chat history goes through SessionStore instead.
	state StateStore
	// objectives is where the active goals and plans live. The reminder is
	// built from this store and scoped to the chat session.
	objectives goals.Store
	// turnMu guards turns, the set of chats with a handler currently running.
	// A reconnecting phone can resend a message whose ack was lost while the
	// first turn still runs; the duplicate must not start a second answer.
	turnMu sync.Mutex
	turns  map[string]bool
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
		turns:     map[string]bool{},
	}
}

// beginTurn claims a chat for one handling turn. Direct HTTP callers and the
// mobile socket share the same gateway, so both are covered.
func (g *Gateway) beginTurn(sessionID string) bool {
	if sessionID == "" {
		return true
	}
	g.turnMu.Lock()
	defer g.turnMu.Unlock()
	if g.turns == nil {
		g.turns = map[string]bool{}
	}
	if g.turns[sessionID] {
		return false
	}
	g.turns[sessionID] = true
	return true
}

func (g *Gateway) endTurn(sessionID string) {
	if sessionID == "" {
		return
	}
	g.turnMu.Lock()
	delete(g.turns, sessionID)
	g.turnMu.Unlock()
}

// SetStartedAt records when this engine process began, so /healthz can report
// how long the daemon has been up.
func (g *Gateway) SetStartedAt(unixSeconds int64)                     { g.startedAt = unixSeconds }
func (g *Gateway) SetLogReader(fn func(int, string) []map[string]any) { g.logReader = fn }

// StateStore is the engine's key/value state: the settings blob the phone
// saves. D1's system_state is the production implementation.
type StateStore interface {
	GetState(key string) (string, bool, error)
	PutState(key, value string) error
}

// SetStateStore hands the gateway the state store opened at boot.
func (g *Gateway) SetStateStore(s StateStore) { g.state = s }

// SetObjectiveStore hands the gateway the goals and plans store opened at boot.
// Without it goal and plan tools still exist but cannot run, and no reminder is
// injected.
func (g *Gateway) SetObjectiveStore(s goals.Store) { g.objectives = s }

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version, "started": g.startedAt})
	})
	mux.HandleFunc("POST /restart", g.apiRestart)
	mux.HandleFunc("GET /v1/providers", g.listProviders)
	mux.HandleFunc("GET /v1/models", g.listModels)
	mux.HandleFunc("POST /v1/sessions", g.createSession)
	mux.HandleFunc("GET /v1/sessions", g.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", g.getSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", g.deleteSession)
	mux.HandleFunc("PATCH /v1/sessions/{id}", g.updateSession)
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
			"keys":     len(provider.Keys),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

type createSessionRequest struct {
	// ID lets the phone promote a locally-created draft without changing the
	// id the screen and its pending messages already use. A second create with
	// the same id is idempotent, so a retried HTTP request cannot make twins.
	ID          string   `json:"id,omitempty"`
	Title       string   `json:"title"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
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
	requested := strings.TrimSpace(body.ID)
	if requested != "" {
		if !sessionIDPattern.MatchString(requested) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "session id must be letters, digits, '.', '_' or '-'")
			return
		}
		// Promoting a draft may be retried after a lost response. Returning the
		// row that already exists makes that retry harmless.
		if found, err := g.sessions.Get(requested); err == nil {
			writeJSON(w, http.StatusOK, found)
			return
		} else if !errors.Is(err, session.ErrNotFound) {
			writeError(w, http.StatusBadGateway, "database_error", err.Error())
			return
		}
	}
	created, err := g.sessions.Create(session.NewSession{
		ID:          requested,
		Title:       body.Title,
		Provider:    body.Provider,
		Model:       body.Model,
		Reasoning:   body.Reasoning,
		Temperature: body.Temperature,
		TopP:        body.TopP,
		MaxTokens:   body.MaxTokens,
	})
	if err != nil {
		// Two requests racing with the same draft id: the loser still gets the
		// one row the winner made.
		if requested != "" {
			if found, getErr := g.sessions.Get(requested); getErr == nil {
				writeJSON(w, http.StatusOK, found)
				return
			}
		}
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

func (g *Gateway) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	_ = decodeBody(r, &body)
	found, err := g.sessions.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if body.Title != "" {
		found.Title = body.Title
	}
	if err := g.sessions.SaveConfig(r.PathValue("id"), *found); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (g *Gateway) getSessionTurns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A phone opens a long chat on its newest page and asks for one older page
	// per scroll. Without these two parameters every open sent the whole
	// transcript over the tunnel, which is what made the first frame wait on
	// history the reader had not reached yet. Both are optional: a request with
	// neither is still the full turns list older clients expect.
	beforeSeq, _ := strconv.ParseInt(r.URL.Query().Get("before_seq"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit > 500 {
		limit = 500
	}
	var (
		turns []session.Turn
		err   error
	)
	if limit > 0 {
		if paged, ok := g.sessions.(PagedTurnStore); ok {
			turns, err = paged.TurnsPage(id, beforeSeq, limit)
		} else {
			turns, err = g.sessions.Turns(id)
			turns = turnPage(turns, beforeSeq, limit)
		}
	} else {
		turns, err = g.sessions.Turns(id)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns})
}

// turnPage picks the newest limit turns before beforeSeq from an oldest-first
// transcript. It is the fallback for a store without TurnsPage, and the same
// rule the SQL uses.
func turnPage(turns []session.Turn, beforeSeq int64, limit int) []session.Turn {
	if limit <= 0 {
		return turns
	}
	e := len(turns)
	if beforeSeq > 0 {
		e = 0
		for e < len(turns) && int64(turns[e].Seq) < beforeSeq {
			e++
		}
	}
	s := e - limit
	if s < 0 {
		s = 0
	}
	return turns[s:e]
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

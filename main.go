package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"aixodia-engine/config"
	"aixodia-engine/db"
	"aixodia-engine/io/gateway"
	"aixodia-engine/io/mobile"
	"aixodia-engine/io/relay"
	"aixodia-engine/io/terminal"
	"aixodia-engine/localdb"
	"aixodia-engine/provider/registry"
	"aixodia-engine/selfupdate"
	"aixodia-engine/session"
	"aixodia-engine/tools"
)

var (
	sessionStore gateway.SessionStore
	// storeDB is the handle behind sessionStore: providers, settings and the
	// daemon's own notes live in the same file, but they are not chat history,
	// so they are not reached through the gateway's store interface.
	storeDB *sql.DB
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := strings.TrimSpace(os.Getenv("CF_TOKEN"))
	if token == "" {
		log.Fatal("CF_TOKEN is not set")
	}

	if !db.Verify(token) {
		log.Fatal("CF_TOKEN is invalid")
	}

	// Engine logs live on this machine (local SQLite, rotating). Opening the
	// database is what creates the table, so there is nothing to do before it.
	startD1Logging()
	log.Print("engine started")

	// A newer engine (started later) owns the `daemon/*` keys. If one is already
	// running, this older instance exits instead of fighting it.
	if newer, err := newerDaemonRunning(startedAt); err != nil {
		log.Printf("guard: %v", err)
	} else if newer {
		log.Print("a newer aixodia-engine is already serving; exiting")
		return
	}

	// The store is this machine's own file: chat history, providers with their
	// API keys, settings and the daemon's notes all live in it. D1 keeps the
	// discovery keys and nothing else, and no request waits on it.
	handle, err := localdb.Open(localdb.Path())
	if err != nil {
		log.Fatal(err)
	}
	storeDB = handle
	store := session.NewSQLite(handle)
	sessionStore = store
	// Providers — with the API keys that exist nowhere else — live in D1, not in
	// this file: a fresh session still has to find them. The table was dropped
	// while providers lived locally, so it is put back before the first read.
	config.SetStore(config.NewStore(db.Query))
	if err := config.EnsureStore(); err != nil {
		log.Printf("config: %v", err)
	}

	startConfigReloader(ctx)

	g := gateway.New(store, registry.New(), tools.Builtin())
	g.SetLocalStore(storeDB)
	g.SetStartedAt(startedAt)
	g.SetLogReader(recentLogs)
	// Attachments land beside the store, under ~/.aixodia, because that is the
	// directory the machine snapshot carries across Kaggle sessions. Anything
	// under /kaggle/working is rebuilt on the next session, so a file saved
	// there was gone the moment the kernel restarted. The agent reads
	// attachments from these paths.
	uploadDir := "./uploads"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		uploadDir = filepath.Join(home, ".aixodia", "uploads")
	}
	g.SetUploadDir(uploadDir)
	gw := g.Handler()
	const model = "Opencode/big-pickle"
	// One hub for every phone socket.
	hub := mobile.NewHub()
	mux := http.NewServeMux()
	mux.Handle("/ws", hub.Handler(token, chatViaGateway(gw, model)))
	// The terminal checks the token itself: a pattern this specific wins over
	// the "/" catch-all below, so RequireToken never sees this path.
	termSrv := terminal.NewServer(token)
	mux.Handle("/terminal", termSrv.Handler())
	mux.Handle("/", gateway.RequireToken(token, gw))
	server := &http.Server{
		Addr:    "127.0.0.1:8787",
		Handler: mux,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
			stop()
		}
	}()

	// The public address is the relay room, not a tunnel this process starts:
	// the engine dials out, so it needs no inbound port and no cloudflared.
	const (
		relayBase = "wss://aixodia-relay.aixodia.workers.dev"
		room      = "aixodia"
	)
	relayToken := token
	roomPath := "/room/" + url.PathEscape(room)
	roomURL := strings.TrimRight(relayBase, "/") + roomPath
	publicURL := strings.Replace(strings.Replace(roomURL, "wss://", "https://", 1), "ws://", "http://", 1)

	var relayUp atomic.Bool
	noted := false
	onConnect := func() {
		relayUp.Store(true)
		log.Print("relay: connected")
		if err := db.SaveDaemon(publicURL, versionString(), time.Now().Unix()); err != nil {
			log.Printf("save daemon: %v", err)
			return
		}
		log.Print("daemon url saved")
		// The note names an address that is already reachable, so it is written
		// after the daemon keys; it is written once per process, not on every
		// reconnect.
		if !noted {
			noted = true
			publishHandover(ctx, publicURL)
		}
	}
	onDisconnect := func() { relayUp.Store(false) }

	// RPC requests come from a socket the relay already authenticated, so the
	// gateway is served whole. The phone and terminal sockets are deliberately
	// absent: they are transport, and the relay owns that transport already.
	rpcMux := http.NewServeMux()
	rpcMux.Handle("/", gateway.RequireToken(token, gw))

	log.Printf("relay: dialling %s", roomURL)
	go func() {
		err := relay.Run(ctx, relay.Config{
			URL:          roomURL,
			Token:        relayToken,
			APIToken:     token,
			Started:      startedAt,
			Version:      versionString(),
			Hub:          hub,
			Chat:         chatViaGateway(gw, model),
			API:          rpcMux,
			Term:         termSrv,
			OnConnect:    onConnect,
			OnDisconnect: onDisconnect,
		})
		if errors.Is(err, relay.ErrSuperseded) {
			log.Print("relay: a newer engine took the room; exiting")
			stop()
		}
	}()
	go startHeartbeat(ctx, stop, relayUp.Load)

	// On Kaggle, periodically check for a newer engine binary and restart
	// in place without ending the session.
	if _, err := os.Stat("/kaggle/working"); err == nil {
		go selfupdate.RunLoop(ctx, func() bool { return activeChats.Load() == 0 })
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

func startConfigReloader(ctx context.Context) {
	loadProviders := func() {
		if err := config.LoadConfig(); err != nil {
			log.Printf("config: %v", err)
			// A boot whose CREATE failed would otherwise never recover: retry
			// the schema before the next read. D1 being down fails here too,
			// and the next tick tries again.
			if err := config.EnsureStore(); err != nil {
				log.Printf("config: %v", err)
				return
			}
			if err := config.LoadConfig(); err != nil {
				log.Printf("config: %v", err)
			}
		}
	}
	loadProviders()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				loadProviders()
			}
		}
	}()
}

// chatViaGateway answers one message by calling the HTTP chat route in-process
// with stream=true, so the WebSocket path shares routing, history, and
// persistence with the API. Each SSE delta is forwarded through onDelta.
// The model is the chat's pinned provider/model when one is stored,
// otherwise the agent default — without this every chat is answered with
// the default even after the user pins another model mid-session.
func chatViaGateway(h http.Handler, model string) mobile.ChatFunc {
	return func(ctx context.Context, sessionID, text string, attachments []mobile.Attachment, onDelta func(string), onTool func(mobile.ToolEvent)) (mobile.ChatResult, error) {
		activeChats.Add(1)
		defer activeChats.Add(-1)
		var result mobile.ChatResult
		// The pictures among the attachments are named to the gateway, which
		// loads them and sends them as pictures. It cannot do that from the text
		// alone: the note lists paths, and a path is not a picture.
		payload := []map[string]string(nil)
		for _, a := range attachments {
			payload = append(payload, map[string]string{"path": a.Path, "name": a.Name, "mime": a.Mime})
		}
		body, err := json.Marshal(map[string]any{
			"session_id": sessionID,
			"model":      sessionModel(h, sessionID, model),
			"stream":     true,
			// This frame carries only the message just typed, not the
			// transcript: the gateway appends it to the stored history instead
			// of trying to match a prefix it cannot share.
			"append":      true,
			"messages":    []map[string]string{{"role": "user", "content": text}},
			"attachments": payload,
		})
		if err != nil {
			return result, err
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")

		var full strings.Builder
		var inTok, outTok int
		var streamErr string
		sw := &sseWriter{header: http.Header{}, status: http.StatusOK}
		sw.onEvent = func(data []byte) {
			if bytes.Equal(data, []byte("[DONE]")) {
				return
			}
			if ev, ok := parseToolEvent(data); ok {
				if onTool != nil {
					onTool(ev)
				}
				return
			}
			var ev struct {
				Turns       []session.Turn `json:"turns"`
				Turn        *session.Turn  `json:"turn"`
				TurnsSynced bool           `json:"turns_synced"`
				Choices     []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				return
			}
			result.Turns = append(result.Turns, ev.Turns...)
			if ev.Turn != nil {
				result.Answer = *ev.Turn
				result.TurnsSynced = ev.TurnsSynced
			}
			if ev.Error != nil {
				streamErr = ev.Error.Message
				return
			}
			if ev.Usage != nil {
				inTok, outTok = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
			}
			for _, c := range ev.Choices {
				if c.Delta.Content != "" {
					full.WriteString(c.Delta.Content)
					onDelta(c.Delta.Content)
				}
			}
		}
		h.ServeHTTP(sw, req)
		if sw.status != http.StatusOK {
			return result, fmt.Errorf("chat %d: %s", sw.status, strings.TrimSpace(sw.raw.String()))
		}
		if streamErr != "" {
			return result, fmt.Errorf("chat: %s", streamErr)
		}
		if result.Answer.Text == "" && result.Answer.Seq == 0 {
			result.Answer.Text = full.String()
			result.Answer.InputTokens, result.Answer.OutputTokens = inTok, outTok
		}
		return result, nil
	}
}

// parseToolEvent decodes a gateway tool_event SSE payload into a mobile
// tool event. It reports false when the payload is anything else.
func parseToolEvent(data []byte) (mobile.ToolEvent, bool) {
	var wrapped struct {
		ToolEvent *struct {
			Stage      string `json:"stage"`
			Name       string `json:"name"`
			CallID     string `json:"call_id"`
			Arguments  string `json:"arguments"`
			Text       string `json:"text"`
			IsError    bool   `json:"is_error"`
			DurationMs int64  `json:"duration_ms"`
			Round      int    `json:"round"`
			State      string `json:"state"`
			// Usage rides the stage="usage" event: the turn's running total
			// after a provider round, so the phone's cost strip climbs with
			// the rounds instead of jumping once when the turn lands.
			PromptTokens       int  `json:"prompt_tokens"`
			CompletionTokens   int  `json:"completion_tokens"`
			CacheReadTokens    int  `json:"cache_read_tokens"`
			CacheWriteTokens   int  `json:"cache_write_tokens"`
			ReasoningTokens    int  `json:"reasoning_tokens"`
			InputIncludesCache bool `json:"input_includes_cache"`
		} `json:"tool_event"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil || wrapped.ToolEvent == nil {
		return mobile.ToolEvent{}, false
	}
	te := wrapped.ToolEvent
	return mobile.ToolEvent{
		Stage: te.Stage, Name: te.Name, CallID: te.CallID,
		Arguments: te.Arguments, Text: te.Text,
		IsError: te.IsError, DurationMs: te.DurationMs,
		Round: te.Round, State: te.State,
		PromptTokens: te.PromptTokens, CompletionTokens: te.CompletionTokens,
		CacheReadTokens: te.CacheReadTokens, CacheWriteTokens: te.CacheWriteTokens,
		ReasoningTokens: te.ReasoningTokens, InputIncludesCache: te.InputIncludesCache,
	}, true
}

// sessionModel returns the chat's pinned provider/model when one is stored,
// otherwise the agent default.
func sessionModel(h http.Handler, sessionID, def string) string {
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return def
	}
	var got struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Pinned   bool   `json:"pinned"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		return def
	}
	if !got.Pinned || got.Provider == "" || got.Model == "" {
		return def
	}
	return got.Provider + "/" + got.Model
}

// sseWriter is a minimal in-process http.ResponseWriter that splits the SSE
// body into events as they are written and keeps the raw body for errors.
type sseWriter struct {
	header  http.Header
	status  int
	raw     bytes.Buffer
	pending bytes.Buffer
	onEvent func([]byte)
}

func (w *sseWriter) Header() http.Header { return w.header }

func (w *sseWriter) WriteHeader(code int) { w.status = code }

func (w *sseWriter) Flush() {}

func (w *sseWriter) Write(p []byte) (int, error) {
	w.raw.Write(p)
	w.pending.Write(p)
	for {
		buf := w.pending.Bytes()
		idx := bytes.Index(buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := append([]byte(nil), buf[:idx]...)
		w.pending.Next(idx + 2)
		if bytes.HasPrefix(event, []byte("data: ")) && w.onEvent != nil {
			w.onEvent(event[6:])
		}
	}
	return len(p), nil
}

// startedAt marks this process; a later start is a newer engine.
var startedAt = time.Now().Unix()
var activeChats atomic.Int64

const versionTag = "aixodia-engine/1.1"

// heartbeatDeps are the daemon-key calls a heartbeat makes. They are a struct so
// a test can watch the write without D1.
type heartbeatDeps struct {
	newer func(started int64) (bool, error)
	touch func(version string, at int64) error
	now   func() int64
}

// heartbeatOnce performs one heartbeat and reports whether this engine should
// keep running. While the tunnel is down it stops refreshing daemon/heartbeat:
// the app reads a fresh heartbeat as "online", so touching it with a dead
// tunnel is exactly what makes the phone adopt a URL it cannot dial.
func heartbeatOnce(started int64, alive func() bool, deps heartbeatDeps) bool {
	if !alive() {
		log.Print("heartbeat: relay is down; not refreshing the daemon keys")
		return true
	}
	newer, err := deps.newer(started)
	if err != nil {
		log.Printf("heartbeat guard: %v", err)
	}
	if newer {
		log.Print("heartbeat: a newer aixodia-engine took over; stopping")
		return false
	}
	if err := deps.touch(versionString(), deps.now()); err != nil {
		log.Printf("heartbeat: %v", err)
	}
	return true
}

// startHeartbeat keeps the daemon keys current so the app can find
// this daemon and read its uptime. It only writes while the tunnel is alive,
// and it stops this instance when a newer engine has taken the keys.
func startHeartbeat(ctx context.Context, stop context.CancelFunc, alive func() bool) {
	deps := heartbeatDeps{
		newer: newerDaemonRunning,
		touch: db.TouchDaemon,
		now:   func() int64 { return time.Now().Unix() },
	}
	beat := func() bool {
		if heartbeatOnce(startedAt, alive, deps) {
			return true
		}
		stop()
		return false
	}
	if !beat() {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !beat() {
				return
			}
		}
	}
}

// newerDaemonRunning reports whether the daemon keys were written recently by an
// engine that started after `started`.
func newerDaemonRunning(started int64) (bool, error) {
	rows, err := db.Query("SELECT key, value FROM system_state WHERE key IN ('daemon/version','daemon/heartbeat')")
	if err != nil {
		return false, err
	}
	var version string
	var beat int64
	for _, row := range rows {
		key, _ := row["key"].(string)
		value, _ := row["value"].(string)
		switch key {
		case "daemon/version":
			version = value
		case "daemon/heartbeat":
			beat, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	other := parseStarted(version)
	fresh := time.Now().Unix()-120 < beat
	return fresh && other > started, nil
}

// versionString is the version column the daemon writes: the tag plus the
// unix second this process started, which the phone parses for uptime.
func versionString() string {
	return fmt.Sprintf("%s started=%d", versionTag, startedAt)
}

// parseStarted reads "started=<unix>" from the version column (0 if absent).
func parseStarted(version string) int64 {
	m := regexp.MustCompile(`started=(\d+)`).FindStringSubmatch(version)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

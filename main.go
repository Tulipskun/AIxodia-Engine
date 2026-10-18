package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ai-engine/config"
	"ai-engine/db"
	"ai-engine/io/gateway"
	"ai-engine/io/mobile"
	"ai-engine/localdb"
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
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
		log.Print("a newer ai-engine is already serving; exiting")
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
	// Providers — with the API keys that exist nowhere else — live in that same
	// file. D1's copy is read once by the reloader below, and only while the
	// store holds none.
	config.SetStore(config.NewStore(storeDB))

	startConfigReloader(ctx)

	g := gateway.New(store, registry.New(), tools.Builtin())
	g.SetLocalStore(storeDB)
	g.SetStartedAt(startedAt)
	g.SetLogReader(recentLogs)
	// Attachments land here; the agent reads them from these paths. Kaggle's
	// /kaggle/working is the writable directory, and the env can point it
	// somewhere else (or to nothing, which disables uploads).
	uploads := strings.TrimSpace(os.Getenv("AIXODIA_UPLOADS"))
	if uploads == "" {
		uploads = "/kaggle/working/uploads"
	}
	g.SetUploadDir(uploads)
	gw := g.Handler()
	model := strings.TrimSpace(os.Getenv("AIXODIA_MODEL"))
	if model == "" {
		model = "Opencode/big-pickle"
	}
	// One hub for every phone socket.
	hub := mobile.NewHub()
	mux := http.NewServeMux()
	mux.Handle("/ws", hub.Handler(token, chatViaGateway(gw, model)))
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

	url, err := startTunnel(ctx)
	if err != nil {
		log.Fatalf("tunnel: %v", err)
	}
	log.Printf("tunnel url: %s", url)

	if os.Getenv("AIXODIA_PUBLISH") == "0" {
		log.Printf("local test mode: tunnel not published to D1")
	} else {
		if err := db.SaveDaemon(url, versionString(), time.Now().Unix()); err != nil {
			log.Fatalf("save daemon: %v", err)
		}
		log.Print("daemon url saved")
		// The note is written after the daemon keys so it names a URL that is
		// already reachable, and before the heartbeat so the record of this
		// session exists even if the next line fails.
		publishHandover(ctx, url)
		go startHeartbeat(ctx, stop)
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
		// ImportLegacyProviders does nothing once the store holds a provider,
		// which is what makes it the retry for a boot whose read of D1 failed:
		// a provider set is never half-carried.
		if _, err := config.ImportLegacyProviders(db.Query); err != nil {
			log.Printf("config: %v", err)
		}
		if err := config.LoadConfig(); err != nil {
			log.Printf("config: %v", err)
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

var tunnelURL = regexp.MustCompile(`https://[A-Za-z0-9.-]+\.trycloudflare\.com`)

// startTunnel publishes localhost:8787 through a Cloudflare quick tunnel
// and returns the random public URL. It needs the cloudflared binary.
func startTunnel(ctx context.Context) (string, error) {
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", errors.New("cloudflared not found in PATH")
	}
	child := context.Background()
	cmd := exec.CommandContext(child, path, "tunnel", "--no-autoupdate",
		"--metrics", "127.0.0.1:0", "--url", "http://127.0.0.1:8787")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			if match := tunnelURL.FindString(scanner.Text()); match != "" {
				select {
				case found <- match:
				default:
				}
			}
		}
	}()
	select {
	case url := <-found:
		go func() {
			<-ctx.Done()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		return url, nil
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("timed out waiting for the trycloudflare URL")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", ctx.Err()
	}
}

// chatViaGateway answers one message by calling the HTTP chat route in-process
// with stream=true, so the WebSocket path shares routing, history, and
// persistence with the API. Each SSE delta is forwarded through onDelta.
// The model is the chat's pinned provider/model when one is stored,
// otherwise the agent default — without this every chat is answered with
// the default even after the user pins another model mid-session.
func chatViaGateway(h http.Handler, model string) mobile.ChatFunc {
	return func(ctx context.Context, sessionID, text string, attachments []mobile.Attachment, onDelta func(string), onTool func(mobile.ToolEvent)) (mobile.ChatResult, error) {
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

const versionTag = "ai-engine/1.1"

// startHeartbeat keeps the daemon keys current so the app can find
// this daemon and read its uptime. Before each write it checks whether a newer
// engine has taken them; if so it calls stop so this older instance shuts
// down.
func startHeartbeat(ctx context.Context, stop context.CancelFunc) {
	beat := func() bool {
		newer, err := newerDaemonRunning(startedAt)
		if err != nil {
			log.Printf("heartbeat guard: %v", err)
		}
		if newer {
			log.Print("heartbeat: a newer ai-engine took over; stopping")
			stop()
			return false
		}
		if err := db.TouchDaemon(versionString(), time.Now().Unix()); err != nil {
			log.Printf("heartbeat: %v", err)
		}
		return true
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

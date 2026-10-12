package main

import (
	"bufio"
	"bytes"
	"context"
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
	"ai-engine/provider/registry"
	"ai-engine/session"
	"ai-engine/tools"
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

	// A newer engine (started later) owns the `nodes` row. If one is already
	// running, this older instance exits instead of fighting it.
	if newer, err := newerDaemonRunning(startedAt); err != nil {
		log.Printf("guard: %v", err)
	} else if newer {
		log.Print("a newer ai-engine is already serving; exiting")
		return
	}

	startConfigReloader(ctx)

	gw := gateway.New(session.NewD1(), registry.New(), tools.Builtin()).Handler()
	model := strings.TrimSpace(os.Getenv("AIXODIA_MODEL"))
	if model == "" {
		model = "Opencode/big-pickle"
	}
	mux := http.NewServeMux()
	mux.Handle("/ws", mobile.Handler(token, chatViaGateway(gw, model)))
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
		if err := db.SaveTunnel(url); err != nil {
			log.Fatalf("save tunnel: %v", err)
		}
		log.Print("tunnel url saved")
		go startHeartbeat(ctx, stop, url)
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

func startConfigReloader(ctx context.Context) {
	if err := config.LoadConfig(); err != nil {
		log.Printf("config: %v", err)
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := config.LoadConfig(); err != nil {
					log.Printf("config: %v", err)
				}
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
	return func(ctx context.Context, sessionID, text string, onDelta func(string)) (string, int, int, error) {
		body, err := json.Marshal(map[string]any{
			"session_id": sessionID,
			"model":      sessionModel(h, sessionID, model),
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": text}},
		})
		if err != nil {
			return "", 0, 0, err
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
			var ev struct {
				Choices []struct {
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
			return "", 0, 0, fmt.Errorf("chat %d: %s", sw.status, strings.TrimSpace(sw.raw.String()))
		}
		if streamErr != "" {
			return "", 0, 0, fmt.Errorf("chat: %s", streamErr)
		}
		return full.String(), inTok, outTok, nil
	}
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

// startHeartbeat keeps the single `nodes` row ('ai') current so the app can
// find this daemon. Before each write it checks whether a newer engine has
// taken the row; if so it calls stop so this older instance shuts down.
func startHeartbeat(ctx context.Context, stop context.CancelFunc, url string) {
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
		_, err = db.Query(
			"INSERT INTO nodes (id, tunnel_url, version, heartbeat) VALUES ('ai', ?, ?, ?) "+
				"ON CONFLICT(id) DO UPDATE SET tunnel_url = excluded.tunnel_url, version = excluded.version, heartbeat = excluded.heartbeat",
			url, fmt.Sprintf("%s started=%d", versionTag, startedAt), time.Now().Unix())
		if err != nil {
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

// newerDaemonRunning reports whether the `nodes` row was written recently by
// an engine that started after `started`.
func newerDaemonRunning(started int64) (bool, error) {
	rows, err := db.Query("SELECT version, heartbeat FROM nodes WHERE id = 'ai'")
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	version, _ := rows[0]["version"].(string)
	beat, _ := rows[0]["heartbeat"].(float64)
	other := parseStarted(version)
	fresh := float64(time.Now().Unix()-120) < beat
	return fresh && other > started, nil
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

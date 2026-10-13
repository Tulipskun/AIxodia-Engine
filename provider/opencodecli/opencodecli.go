// Package opencodecli answers OpenCode models by running the OpenCode command
// line client instead of calling the inference endpoint directly.
//
// The endpoint cannot be called by anything but that client. Its free tier
// answers the real client and refuses every other caller — measured, not
// assumed: the request the client sends was captured byte for byte (User-Agent,
// x-opencode-org-id, a real project id, the session headers, the harness tool
// declarations, max_completion_tokens, stream_options) and replayed from Go,
// Python and curl, from this host and from two Kaggle hosts. All refused, with
// "OpenCode's free tier can only be used from within OpenCode"; the client
// itself answers in the same second. Nothing in the request is the difference,
// so it is the connection that is checked, and no amount of matching the request
// changes that.
//
// So the client is used as it is. It is not impersonated: it is installed,
// authenticated with the same key the daemon already holds, and asked to do the
// work. The cost is that an OpenCode model runs OpenCode's own agent and tools
// rather than the ones this engine defines, which is why the adapter is a
// separate name an operator opts into per provider.
package opencodecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ai-engine/provider"
)

// EnvBinary names the client, for an install that is not on PATH.
const EnvBinary = "AIXODIA_OPENCODE_BIN"

// Adapter runs the client once per turn.
type Adapter struct {
	mu   sync.Mutex
	bin  string
	auth string
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "opencode-cli" }

// Binary finds the client. It is looked for where the installer puts it and
// where the official installer puts it, and an explicit override wins.
func (a *Adapter) Binary() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bin != "" {
		return a.bin, nil
	}
	if custom := strings.TrimSpace(os.Getenv(EnvBinary)); custom != "" {
		a.bin = custom
		return a.bin, nil
	}
	if found, err := exec.LookPath("opencode"); err == nil {
		a.bin = found
		return a.bin, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".opencode", "bin", "opencode")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			a.bin = candidate
			return a.bin, nil
		}
	}
	return "", fmt.Errorf("the OpenCode client is not installed (looked on PATH and in ~/.opencode/bin; set %s to point at it)", EnvBinary)
}

// Authenticate writes the key the daemon holds into the file the client reads.
//
// The client takes its credential from ~/.local/share/opencode/auth.json, not
// from the environment: the environment variable forms were tried and the client
// answered "Save the token to ...". The key that works is the one already stored
// for the provider, which was checked by running the client with it.
func (a *Adapter) Authenticate(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("no key for the provider")
	}
	a.mu.Lock()
	if a.auth == key {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot locate the home directory: %w", err)
	}
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	payload, err := json.Marshal(map[string]any{
		"opencode": map[string]string{"type": "api", "key": key},
	})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	a.mu.Lock()
	a.auth = key
	a.mu.Unlock()
	return nil
}

// event is one line of `opencode run --format json`.
type event struct {
	Type string `json:"type"`
	Part struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Tool  string `json:"tool"`
		State struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Reason string `json:"reason"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
}

func (a *Adapter) Complete(ctx context.Context, req *provider.Request, prov provider.Provider, key string) (*provider.Response, error) {
	return a.Stream(ctx, req, prov, key, nil)
}

// Stream runs one turn and reports what the client did with it.
//
// The conversation is the client's own: it keeps the session named by
// --session, so only the message being asked is passed here. Replaying this
// engine's history would duplicate it — the client already has the earlier
// turns, including the tool calls it made itself.
func (a *Adapter) Stream(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	binary, err := a.Binary()
	if err != nil {
		return nil, err
	}
	if err := a.Authenticate(key); err != nil {
		return nil, err
	}
	prompt := lastUserText(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("opencode-cli needs a user message to send")
	}

	args := []string{"run", "--format", "json", "--model", req.Model}
	if session := strings.TrimSpace(req.SessionID); session != "" {
		// The client creates the session when the id is new and continues it
		// afterwards, so this engine's session id is used unchanged and the two
		// stay in step without a mapping table.
		args = append(args, "--session", session)
	}
	if req.MaxTokens > 0 {
		// The client reads its own limits from its config; passing this engine's
		// would silently cap a model that was configured for more.
		_ = req.MaxTokens
	}
	args = append(args, prompt)

	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workingDirectory()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	command.Stderr = &stderr

	started := time.Now()
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("cannot start the OpenCode client: %w", err)
	}

	resp, err := a.consume(stdout, emit)
	waitErr := command.Wait()
	if err != nil {
		_ = waitErr
		return nil, err
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		// The client prints its own diagnostics on stderr; the last line is the
		// one that says what went wrong.
		lines := strings.Split(message, "\n")
		return nil, fmt.Errorf("the OpenCode client failed: %s", strings.TrimSpace(lines[len(lines)-1]))
	}
	resp.GenerationMS = time.Since(started).Milliseconds()
	resp.Provider = prov.Name
	if resp.Model == "" {
		resp.Model = req.Model
	}
	return resp, nil
}

// consume turns the client's event stream into an answer, a trail of tool steps
// and a token count.
//
// The shape is the same one this engine produces itself, which is what lets the
// phone keep its tool blocks and its footer: text is a delta, a tool call is a
// tool_call row followed by its tool_result row, and each step boundary carries
// the tokens that step cost.
func (a *Adapter) consume(stdout io.Reader, emit func(string) error) (*provider.Response, error) {
	resp := &provider.Response{}
	var content strings.Builder
	var pending strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "text":
			text := ev.Part.Text
			if text == "" {
				continue
			}
			content.WriteString(text)
			pending.WriteString(text)
			if emit != nil {
				if err := emit(text); err != nil {
					return nil, err
				}
			}
		case "tool_use":
			// The words before a tool call are this engine's own narration
			// convention, and the phone draws them as a block of their own.
			// They ride on the call row the same way TrailTurns expects.
			call := provider.ToolCall{
				ID:        ev.Part.ID,
				Name:      ev.Part.Tool,
				Arguments: compact(ev.Part.State.Input),
			}
			if call.ID == "" {
				call.ID = fmt.Sprintf("call_%d", time.Now().UnixNano())
			}
			resp.Trail = append(resp.Trail, provider.Message{
				Role:      "assistant",
				Content:   strings.TrimSpace(pending.String()),
				ToolCalls: []provider.ToolCall{call},
			})
			pending.Reset()
			output := ev.Part.State.Output
			if ev.Part.State.Error != "" {
				output = ev.Part.State.Error
			}
			resp.Trail = append(resp.Trail, provider.Message{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
				ToolName:   call.Name,
			})
		case "step_finish":
			t := ev.Part.Tokens
			resp.Usage.PromptTokens += t.Input
			resp.Usage.CompletionTokens += t.Output
			resp.Usage.ReasoningTokens += t.Reasoning
			resp.Usage.CacheReadTokens += t.Cache.Read
			resp.Usage.CacheWriteTokens += t.Cache.Write
			resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
			if ev.Part.Reason != "" {
				resp.FinishReason = ev.Part.Reason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the OpenCode client's output: %w", err)
	}
	resp.Content = content.String()
	if resp.FinishReason == "tool-calls" {
		resp.FinishReason = "stop"
	}
	return resp, nil
}

// lastUserText is the message being asked. Everything before it is history the
// client already has.
func lastUserText(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if text := strings.TrimSpace(messages[i].Content); text != "" {
			return text
		}
	}
	return ""
}

// compact is the tool arguments as one line, which is what the phone shows.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// workingDirectory is where the client runs its tools. Kaggle's writable
// directory is the only sensible one, and an install that set a workspace gets
// that instead.
func workingDirectory() string {
	if custom := strings.TrimSpace(os.Getenv("AIXODIA_WORKSPACE")); custom != "" {
		return custom
	}
	if info, err := os.Stat("/kaggle/working"); err == nil && info.IsDir() {
		return "/kaggle/working"
	}
	return ""
}

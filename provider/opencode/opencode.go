package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"ai-engine/provider"
	"ai-engine/provider/openai"
)

const clientID = "opencode/1.18.31"

const (
	shellTool = "shell"
	readTool  = "read"
)

const b62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

type Adapter struct {
	inner   *openai.Adapter
	session string
}

func New() *Adapter {
	a := &Adapter{session: newID("ses_")}
	a.inner = &openai.Adapter{HeaderFunc: a.headers}
	return a
}

func (a *Adapter) Name() string { return "opencode" }

func (a *Adapter) headers() map[string]string {
	return map[string]string{
		"User-Agent":         clientID,
		"x-opencode-client":  "cli",
		"x-opencode-project": "global",
		"x-opencode-session": a.session,
		"x-opencode-request": newID("msg_"),
	}
}

func (a *Adapter) Complete(ctx context.Context, req *provider.Request, prov provider.Provider, key string) (*provider.Response, error) {
	var content strings.Builder
	resp, err := a.Stream(ctx, req, prov, key, func(delta string) error {
		content.WriteString(delta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp.Content = content.String()
	return resp, nil
}

func (a *Adapter) Stream(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	prepared := *req
	prepared.Tools = contractTools(req.Tools)
	resp, err := a.inner.Stream(ctx, &prepared, prov, key, emit)
	if err != nil && len(prepared.Tools) > 0 && isToolProtocolError(err) {
		// Some models do not accept function-calling payloads at all; retry
		// once without any tools rather than failing the whole request.
		stripped := *req
		stripped.Tools = nil
		return a.inner.Stream(ctx, &stripped, prov, key, emit)
	}
	return resp, err
}

// isToolProtocolError reports whether err looks like an upstream rejection of
// the function-calling payload itself (as opposed to auth, credits, or model
// availability), in which case a tools-less retry is worth one attempt.
func isToolProtocolError(err error) bool {
	var api *provider.APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.StatusCode != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(api.Message)
	if strings.Contains(msg, "protocol") {
		return true
	}
	hasToolWord := strings.Contains(msg, "tool") || strings.Contains(msg, "function")
	hasRejectWord := strings.Contains(msg, "support") || strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "not allowed") || strings.Contains(msg, "refuse")
	return hasToolWord && hasRejectWord
}

func contractTools(tools []provider.ToolDef) []provider.ToolDef {
	present := make(map[string]bool, len(tools))
	for _, def := range tools {
		present[def.Name] = true
	}
	out := append([]provider.ToolDef(nil), tools...)
	if !present[shellTool] && !present["bash"] {
		out = append(out, placeholder(shellTool))
	}
	if !present[readTool] {
		out = append(out, placeholder(readTool))
	}
	return out
}

func placeholder(name string) provider.ToolDef {
	// The zen free-tier contract expects shell/read to be declared. They are
	// executed for real by the gateway (see tools.Builtin), so describe them
	// truthfully instead of discouraging the model from calling them.
	if name == shellTool {
		return provider.ToolDef{
			Name:        shellTool,
			Description: "Executes a shell command on the host (sh -c) and returns combined stdout and stderr output.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "Shell command to execute."},
				},
				"required": []any{"command"},
			},
		}
	}
	return provider.ToolDef{
		Name:        readTool,
		Description: "Reads a text file from the host filesystem and returns its contents.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute path of the file to read."},
			},
			"required": []any{"path"},
		},
	}
}

func newID(prefix string) string {
	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		now := time.Now().UnixNano()
		for i := range raw {
			raw[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	suffix := make([]byte, 14)
	for i := 0; i < len(suffix); i++ {
		suffix[i] = b62[int(raw[6+i])%len(b62)]
	}
	return prefix + hex.EncodeToString(raw[:6]) + string(suffix)
}

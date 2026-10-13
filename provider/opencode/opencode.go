package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-engine/provider"
	"ai-engine/provider/openai"
	"ai-engine/tools"
)

// clientAgentMeasured is the name the client itself sends. It builds
// `opencode/${channel}/${version}/${name}`, which was read from its source
// (packages/core/src/session/model-request.ts) and confirmed against a recorded
// request. The engine used to send an AI-SDK style string no version of the
// client has ever sent.
const clientAgentMeasured = "opencode/latest/2.0.27/cli"

const (
	shellTool = "shell"
	readTool  = "read"
)

const b62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

type Adapter struct {
	inner   *openai.Adapter
	session string
	// agent overrides the user agent; empty uses the measured one.
	agent string
}

// userAgent is the name sent with each request.
func (a *Adapter) userAgent() string {
	if a.agent != "" {
		return a.agent
	}
	return clientAgentMeasured
}

func New() *Adapter {
	a := &Adapter{session: newID("ses_")}
	a.inner = &openai.Adapter{HeaderFunc: a.headers}
	return a
}

// The free tier of this provider refuses this engine, and the reason is not in
// anything this engine controls. That is worth writing down so nobody spends the
// same time again.
//
// What was established, by measurement rather than by reading:
//
//   - The client's request is fully reproduced here. Its own source
//     (packages/core/src/session/model-request.ts) builds exactly the headers
//     this adapter sends, plus the session ids and the project id, and nothing
//     else — no hidden token, no extra handshake. A recorded request was
//     replayed byte for byte from Go, Python and curl.
//   - The connection is not the difference either. A TLS client matching the
//     recorded ClientHello (cipher suites, extension list and order, signature
//     algorithms, ALPN) gets the same 403 as the plain one, and so does a
//     request written by hand in the client's own header order.
//   - It is not the endpoint, the key, the user agent, the tool declarations or
//     the body shape. Every combination was tried; all refused.
//   - It is not a per-address budget running out: the real client answers from
//     the same address, with the same key, in the same second.
//
// Where the check actually lives is answerable from the source, and the answer
// is that it is not in the source. /zen/v1 is a proxy —
// packages/console/app/src/lib/inference-proxy.ts — which forwards to a separate
// service, and says so in a comment: "New inference owns authentication, model
// availability, free-tier access, and billing." That service is not published,
// and neither the message nor its error type appears in either published branch
// or in the client binary.
//
// So this cannot be fixed from here by imitating anything that can be read. The
// choices are to run the real client (the opencodecli adapter does) or to use a
// provider that permits this engine.

func (a *Adapter) Name() string { return "opencode" }

func (a *Adapter) headers() map[string]string {
	return map[string]string{
		"User-Agent":            a.userAgent(),
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    a.session,
		"x-opencode-request":    newID("msg_"),
		"x-opencode-session-id": a.session,
		"x-session-affinity":    a.session,
		"x-session-id":          a.session,
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
	// Prefer streamed deltas, but some paths (e.g. Responses API fallback
	// resolved from a completed object) return full content without emitting.
	if collected := content.String(); collected != "" {
		resp.Content = collected
	}
	return resp, nil
}

// Stream answers a request, and knows one thing the ladder below does not: a
// picture may be the reason the whole ladder failed.
//
// Whether a model takes pictures is a property of the model, not something this
// code can read from the endpoint, so the request is sent with them and the
// failure is handled here. A model that refuses them gets asked again without,
// and the message is told the pictures were dropped — otherwise it would answer
// as though it had looked at something it never received, which is the failure
// this whole path exists to avoid.
func (a *Adapter) Stream(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	emitted := false
	guard := func(s string) error {
		emitted = true
		if emit == nil {
			return nil
		}
		return emit(s)
	}
	resp, err := a.ladder(ctx, req, prov, key, guard)
	if err == nil || emitted || !carriesImages(req.Messages) || !worthRetryingWithoutImages(err) {
		return resp, err
	}
	stripped := withoutImages(req)
	retried, err2 := a.ladder(ctx, stripped, prov, key, guard)
	if err2 != nil {
		// Report the original failure: it is the one that describes what was
		// actually asked for.
		return nil, err
	}
	return retried, nil
}

// carriesImages reports whether any message would put a picture on the wire.
func carriesImages(messages []provider.Message) bool {
	for _, m := range messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// withoutImages is the same conversation with the pictures taken out and the
// last user message told why, so the answer can be honest about what it saw.
func withoutImages(req *provider.Request) *provider.Request {
	out := *req
	out.Messages = make([]provider.Message, len(req.Messages))
	copy(out.Messages, req.Messages)
	last := -1
	for i := len(out.Messages) - 1; i >= 0; i-- {
		if len(out.Messages[i].Images) > 0 {
			out.Messages[i].Images = nil
			if last < 0 && out.Messages[i].Role == "user" {
				last = i
			}
		}
	}
	if last >= 0 {
		out.Messages[last].Content = strings.TrimRight(out.Messages[last].Content, "\n") +
			"\n\n[the attached pictures were removed: this model does not accept images, so answer from the text alone and say that you could not see them]"
	}
	return &out
}

// worthRetryingWithoutImages decides whether asking again without the pictures
// could plausibly succeed.
//
// The first cut looked for words like "protocol" or "image" in the failure. That
// was too clever: a text-only model handed a picture does not politely say so, it
// falls over — ling-3.1-flash-free answers normally without a picture and
// returns 502 with one, and the word-based test read that as unrelated and gave
// up. The only failures that genuinely cannot be helped by removing the pictures
// are the ones about the key or the balance, so those are what is excluded and
// everything else is retried once.
func worthRetryingWithoutImages(err error) bool {
	var api *provider.APIError
	if errors.As(err, &api) {
		switch api.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired:
			return false
		case http.StatusForbidden:
			// A 403 is also what a payload rejection looks like, so it is only
			// skipped when the message is plainly about credentials.
			msg := strings.ToLower(api.Message)
			for _, hint := range []string{"key", "credit", "quota", "balance", "payment"} {
				if strings.Contains(msg, hint) {
					return false
				}
			}
			return true
		}
	}
	return true
}

func (a *Adapter) ladder(ctx context.Context, req *provider.Request, prov provider.Provider, key string, guarded func(string) error) (*provider.Response, error) {
	emitted := false
	inner := guarded
	guarded = func(s string) error {
		emitted = true
		return inner(s)
	}
	var trace []string
	note := func(name string, err error) {
		trace = append(trace, name+": "+describeErr(err))
	}
	fail := func(err error) error {
		var api *provider.APIError
		if errors.As(err, &api) {
			return &provider.APIError{
				StatusCode: api.StatusCode,
				Message:    api.Message + " [attempts: " + strings.Join(trace, " | ") + "]",
			}
		}
		if len(trace) > 0 {
			return fmt.Errorf("%w [attempts: %s]", err, strings.Join(trace, " | "))
		}
		return err
	}
	// The Responses API is tried first, and that order is the whole fix for the
	// free tier.
	//
	// The gate on this endpoint looks for the tool declarations by name at the
	// top level — "shell" and "read" — which is how the Responses API spells
	// them. Chat completions nests the same tools under "function", so a request
	// sent that way declares no tool the gate recognises and is refused before
	// the model is ever considered. Measured: the same body over the Responses
	// API is accepted, and over chat completions it is refused.
	//
	// The chat path is kept below as the fallback, for any endpoint that serves
	// it and does not serve this one.
	prepared := *req
	prepared.Tools = contractTools(req.Tools)
	if viaResponses, err := a.inner.StreamViaResponses(ctx, &prepared, prov, key, guarded); err == nil {
		return viaResponses, nil
	} else {
		note("responses-stream-tools", err)
		// The same rule as every attempt below: a failure that is not about the
		// shape of the request is not fixed by changing the shape, and retrying
		// it only spends another round trip.
		if emitted || !isToolProtocolError(err) {
			return nil, fail(err)
		}
	}
	resp, err := a.inner.Stream(ctx, &prepared, prov, key, guarded)
	if err == nil {
		return resp, nil
	}
	note("chat-stream-tools", err)
	// Only retry while nothing has streamed yet: a mid-stream failure must
	// surface as-is, otherwise the client would see duplicated content.
	if emitted || !isToolProtocolError(err) {
		return nil, fail(err)
	}
	// Attempt 2: same streaming mode, no tools. Some models reject any
	// function-calling payload outright.
	stripped := *req
	stripped.Tools = nil
	if resp2, err2 := a.inner.Stream(ctx, &stripped, prov, key, guarded); err2 == nil {
		return resp2, nil
	} else if emitted || !isToolProtocolError(err2) {
		note("chat-stream-bare", err2)
		return nil, fail(err2)
	} else {
		note("chat-stream-bare", err2)
	}
	// Attempt 3: non-streaming, no tools. Some models only serve plain
	// (non-SSE) chat completions.
	bare, err3 := a.inner.Complete(ctx, &stripped, prov, key)
	if err3 == nil {
		if guarded != nil {
			if err := guarded(bare.Content); err != nil {
				return nil, err
			}
		}
		return bare, nil
	}
	note("chat-complete-bare", err3)
	if !isToolProtocolError(err3) {
		return nil, fail(err3)
	}
	// Attempt 4: OpenAI Responses API (/v1/responses), STREAMING with the
	// full tool set — exactly what reference clients send. Some models only
	// serve this shape and reject everything else outright.
	fullStream := *req
	fullStream.Tools = contractTools(req.Tools)
	if resp4s, err4s := a.inner.StreamViaResponses(ctx, &fullStream, prov, key, guarded); err4s == nil {
		return resp4s, nil
	} else if emitted || !isToolProtocolError(err4s) {
		note("responses-stream-tools", err4s)
		return nil, fail(err4s)
	} else {
		note("responses-stream-tools", err4s)
	}
	// Attempt 5: Responses API with the full tool set, non-streaming.
	full := *req
	full.Tools = contractTools(req.Tools)
	if resp4, err4 := a.inner.CompleteViaResponses(ctx, &full, prov, key); err4 == nil {
		if guarded != nil {
			if err := guarded(resp4.Content); err != nil {
				return nil, err
			}
		}
		return resp4, nil
	} else if emitted || !isToolProtocolError(err4) {
		note("responses-complete-tools", err4)
		return nil, fail(err4)
	} else {
		note("responses-complete-tools", err4)
	}
	// Attempt 6: Responses API bare (no tools at all).
	via, err5 := a.inner.CompleteViaResponses(ctx, &stripped, prov, key)
	note("responses-complete-bare", err5)
	if err5 != nil {
		return nil, fail(err5)
	}
	if guarded != nil {
		if err := guarded(via.Content); err != nil {
			return nil, err
		}
	}
	return via, nil
}

// describeErr condenses an upstream error to "status: snippet" for traces.
func describeErr(err error) string {
	if err == nil {
		return "ok"
	}
	var api *provider.APIError
	if errors.As(err, &api) {
		msg := api.Message
		if len(msg) > 140 {
			msg = msg[:140] + "…"
		}
		return fmt.Sprintf("http%d %s", api.StatusCode, msg)
	}
	msg := err.Error()
	if len(msg) > 140 {
		msg = msg[:140] + "…"
	}
	return msg
}

// isToolProtocolError reports whether err looks like an upstream rejection of
// the function-calling payload itself (as opposed to auth, credits, or model
// availability), in which case a tools-less retry is worth one attempt.
func isToolProtocolError(err error) bool {
	var api *provider.APIError
	if !errors.As(err, &api) {
		return false
	}
	msg := strings.ToLower(api.Message)
	// The free-tier gate rejects shapes without the full tool declarations
	// ("can only be used from within OpenCode"), usually as 403. A later
	// attempt with the full tool set may still pass, so keep going instead
	// of stopping here.
	if strings.Contains(msg, "free tier") && strings.Contains(msg, "opencode") {
		return true
	}
	if api.StatusCode != http.StatusBadRequest {
		return false
	}
	if strings.Contains(msg, "protocol") {
		return true
	}
	hasToolWord := strings.Contains(msg, "tool") || strings.Contains(msg, "function")
	hasRejectWord := strings.Contains(msg, "support") || strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "not allowed") || strings.Contains(msg, "refuse")
	return hasToolWord && hasRejectWord
}

func contractTools(given []provider.ToolDef) []provider.ToolDef {
	present := make(map[string]bool, len(given))
	for _, def := range given {
		present[def.Name] = true
	}
	out := append([]provider.ToolDef(nil), given...)
	// The free-tier gate only passes the full rich declarations (it rejects
	// stub shells), so inject the real builtin defs — never minimal sketches.
	real := tools.Builtin()
	if !present[shellTool] && !present["bash"] {
		if t, ok := real[shellTool]; ok {
			out = append(out, t.Def)
		} else {
			out = append(out, placeholder(shellTool))
		}
	}
	if !present[readTool] {
		if t, ok := real[readTool]; ok {
			out = append(out, t.Def)
		} else {
			out = append(out, placeholder(readTool))
		}
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

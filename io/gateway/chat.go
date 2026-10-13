package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-engine/config"
	"ai-engine/provider"
	"ai-engine/session"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Provider    string        `json:"provider"`
	Messages    []chatMessage `json:"messages"`
	SessionID   string        `json:"session_id"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature"`
	TopP        *float64      `json:"top_p"`
	MaxTokens   int           `json:"max_tokens"`
	Tools       *[]string     `json:"tools"`
	User        string        `json:"user"`
	// Append says the messages are the new turns only, not a transcript. The
	// WebSocket sends one new message per turn; without this flag the history is
	// matched against a single message, never matches, and is discarded.
	Append bool `json:"append,omitempty"`
	// Attachments are files the phone uploaded first. Pictures among them are
	// sent to the model as pictures rather than described to it, so a question
	// about a photograph can be answered by looking at it.
	Attachments []chatAttachment `json:"attachments,omitempty"`
}

type route struct {
	adapter provider.Adapter
	prov    provider.Provider
	model   string
}

type completionOptions struct {
	maxTokens   int
	temperature *float64
	topP        *float64
	tools       []provider.ToolDef
	stream      bool
	user        string
	emit        func(string) error
	// Live tool activity, reported around each execution. Nil when nobody
	// watches (non-streaming API); the streaming API and the phone socket
	// forward them so tool use is visible while it runs.
	onToolCall   func(call provider.ToolCall)
	onToolResult func(call provider.ToolCall, text string, durationMs int64, isError bool)
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	var body chatRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(body.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
		return
	}

	var stored *session.Session
	if body.SessionID != "" {
		found, err := g.sessions.Get(body.SessionID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		stored = found
	}

	incoming := make([]provider.Message, 0, len(body.Messages))
	for _, message := range body.Messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		incoming = append(incoming, provider.Message{Role: role, Content: message.Content})
	}
	// Pictures ride on the message that asked about them. The text note the
	// phone sends alongside still names every file, so a model that cannot take
	// pictures, and every later turn that reads this back from the database, is
	// no worse off than before.
	if withImages, skipped := g.attachImages(incoming, body.Attachments); len(withImages) > 0 {
		incoming = withImages
		for _, reason := range skipped {
			log.Printf("gateway: attachment not sent as a picture: %s", reason)
		}
	}

	history, delta := incoming, incoming
	if stored != nil {
		saved, err := g.sessions.History(stored.ID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "database_error", err.Error())
			return
		}
		if body.Append {
			history, delta = appendDelta(saved, incoming)
		} else {
			history, delta = mergeHistory(saved, incoming)
		}
	}

	primary, err := g.resolveRoute(stored, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	toolset, err := g.selectTools(body.Tools)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	options := completionOptions{
		maxTokens:   firstNonZero(body.MaxTokens, storedMaxTokens(stored)),
		temperature: firstNonZeroPtr(body.Temperature, storedTemperature(stored)),
		topP:        firstNonZeroPtr(body.TopP, storedTopP(stored)),
		tools:       toolset,
		stream:      body.Stream,
		user:        body.User,
	}
	fallback := g.fallbackRoute(stored)

	if body.Stream {
		g.chatStream(w, r, stored, primary, fallback, history, delta, options)
		return
	}
	g.chatOnce(w, r, stored, primary, fallback, history, delta, options)
}

func (g *Gateway) chatOnce(w http.ResponseWriter, r *http.Request, stored *session.Session, primary route, fallback *route, history, delta []provider.Message, options completionOptions) {
	started := time.Now()
	g.persistUserBefore(stored, history, delta)
	resp, err := g.complete(r.Context(), primary, fallback, history, options)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	g.persist(stored, delta, resp, options, time.Since(started).Milliseconds())

	id := resp.ID
	if id == "" {
		id = newCompletionID()
	}
	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"object":   "chat.completion",
		"created":  time.Now().Unix(),
		"model":    resp.Model,
		"provider": resp.Provider,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": resp.Content},
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	})
}

func (g *Gateway) chatStream(w http.ResponseWriter, r *http.Request, stored *session.Session, primary route, fallback *route, history, delta []provider.Message, options completionOptions) {
	flusher, _ := w.(http.Flusher)
	created := time.Now().Unix()
	id := newCompletionID()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	modelID := primary.model
	var buffered strings.Builder
	first := true
	options.emit = func(text string) error {
		buffered.WriteString(text)
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelID,
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         streamDelta(first, text),
					"finish_reason": nil,
				},
			},
		}
		first = false
		return writeSSE(w, flusher, chunk)
	}

	started := time.Now()
	g.persistUserBefore(stored, history, delta)
	options.onToolCall = func(call provider.ToolCall) {
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "tool_call", "name": call.Name, "call_id": call.ID, "arguments": call.Arguments,
		}})
	}
	options.onToolResult = func(call provider.ToolCall, text string, durationMs int64, isError bool) {
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "tool_result", "name": call.Name, "call_id": call.ID,
			"text": truncate(text, 4096), "is_error": isError, "duration_ms": durationMs,
		}})
	}
	resp, err := g.complete(r.Context(), primary, fallback, history, options)
	if err != nil {
		payload := map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}}
		_ = writeSSE(w, flusher, payload)
		writeSSEDone(w, flusher)
		return
	}
	g.persist(stored, delta, resp, options, time.Since(started).Milliseconds())

	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	final := map[string]any{
		"id":       id,
		"object":   "chat.completion.chunk",
		"created":  created,
		"model":    resp.Model,
		"provider": resp.Provider,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	}
	_ = writeSSE(w, flusher, final)
	writeSSEDone(w, flusher)
}

func streamDelta(withRole bool, text string) map[string]any {
	delta := map[string]any{"content": text}
	if withRole {
		delta["role"] = "assistant"
	}
	return delta
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (g *Gateway) resolveRoute(stored *session.Session, body chatRequest) (route, error) {
	providerName := body.Provider
	modelID := body.Model
	if stored != nil {
		if providerName == "" {
			providerName = stored.Provider
		}
		if modelID == "" {
			modelID = stored.Model
		}
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return route{}, fmt.Errorf("model is required (use provider/model or the provider field)")
	}

	adapterFor := func(cfg config.Provider) (provider.Adapter, error) {
		return g.registry.Require(cfg.Adapter)
	}

	if providerName != "" {
		cfg, ok := config.GetProviderByName(providerName)
		if !ok {
			return route{}, fmt.Errorf("unknown provider %q (known: %s)", providerName, providerNames())
		}
		implementation, err := adapterFor(cfg)
		if err != nil {
			return route{}, err
		}
		model := trimProviderPrefix(modelID, cfg.Name)
		if model == "" {
			return route{}, fmt.Errorf("model is required for provider %q", cfg.Name)
		}
		return route{adapter: implementation, prov: toInternalProvider(cfg), model: model}, nil
	}

	if prefix, rest, found := strings.Cut(modelID, "/"); found {
		if cfg, ok := config.GetProviderByName(prefix); ok {
			implementation, err := adapterFor(cfg)
			if err != nil {
				return route{}, err
			}
			return route{adapter: implementation, prov: toInternalProvider(cfg), model: rest}, nil
		}
	}
	return route{}, fmt.Errorf("unknown model %q: prefix it with a provider, for example provider/%s (known: %s)", modelID, modelID, providerNames())
}

func (g *Gateway) fallbackRoute(stored *session.Session) *route {
	if stored == nil || !stored.SubEnabled || stored.SubProvider == "" || stored.SubModel == "" {
		return nil
	}
	cfg, ok := config.GetProviderByName(stored.SubProvider)
	if !ok {
		return nil
	}
	implementation, err := g.registry.Require(cfg.Adapter)
	if err != nil {
		return nil
	}
	return &route{adapter: implementation, prov: toInternalProvider(cfg), model: stored.SubModel}
}

func (g *Gateway) complete(ctx context.Context, primary route, fallback *route, history []provider.Message, options completionOptions) (*provider.Response, error) {
	conversation := append([]provider.Message(nil), history...)
	current := primary
	usedFallback := false

	emitted := 0
	emit := options.emit
	if emit != nil {
		emit = func(text string) error {
			emitted++
			return options.emit(text)
		}
	}
	canRetry := func() bool { return !options.stream || emitted == 0 }

	final := &provider.Response{}
	var served string
	// pending is true when the loop stopped on the round limit while the model
	// still wanted tools. Its last reply is then only tool calls, so one more
	// call without tools asks it to answer from what it already has.
	pending := false

	for round := 0; round < maxToolRounds; {
		request := &provider.Request{
			Model:       current.model,
			Messages:    session.Trim(conversation, session.DefaultMaxTokens),
			MaxTokens:   options.maxTokens,
			Temperature: options.temperature,
			TopP:        options.topP,
			Stream:      options.stream,
			Tools:       options.tools,
			User:        options.user,
		}

		callStarted := time.Now()
		resp, err := g.call(ctx, current, request, emit, canRetry)
		// A failed attempt still cost time, and it is time the turn spent
		// talking to a provider, so it counts towards the generation figure.
		final.GenerationMS += time.Since(callStarted).Milliseconds()
		if err != nil {
			if fallback != nil && !usedFallback && canRetry() {
				current = *fallback
				usedFallback = true
				continue
			}
			return nil, err
		}
		round++
		if served == "" {
			served = current.prov.Name
		}

		final.ID = resp.ID
		final.Model = current.model
		final.Provider = served
		// The last round's text is the answer. Earlier rounds are narration
		// before a tool call; they stay with that tool step in the trail.
		// Accumulating them here ran every round together with no separator,
		// so one stored answer read "กำลังเปิดอ่าน...ครับเปิดรูปไม่ขึ้น...ครับ".
		final.Content = resp.Content
		final.Usage.PromptTokens += resp.Usage.PromptTokens
		final.Usage.CompletionTokens += resp.Usage.CompletionTokens
		final.Usage.TotalTokens += resp.Usage.TotalTokens
		final.FinishReason = resp.FinishReason

		pending = len(resp.ToolCalls) > 0
		if !pending {
			break
		}

		assistant := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		conversation = append(conversation, assistant)
		final.Trail = append(final.Trail, assistant)
		for _, call := range resp.ToolCalls {
			if options.onToolCall != nil {
				options.onToolCall(call)
			}
			started := time.Now()
			output := g.runTool(ctx, call)
			if options.onToolResult != nil {
				options.onToolResult(call, output, time.Since(started).Milliseconds(), isToolError(output))
			}
			result := provider.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Content:    output,
			}
			conversation = append(conversation, result)
			final.Trail = append(final.Trail, result)
		}
		final.FinishReason = "tool_calls"
	}

	if pending {
		request := &provider.Request{
			Model:       current.model,
			Messages:    session.Trim(conversation, session.DefaultMaxTokens),
			MaxTokens:   options.maxTokens,
			Temperature: options.temperature,
			TopP:        options.topP,
			Stream:      options.stream,
			User:        options.user,
		}
		callStarted := time.Now()
		resp, err := g.call(ctx, current, request, emit, canRetry)
		final.GenerationMS += time.Since(callStarted).Milliseconds()
		if err != nil {
			return nil, err
		}
		final.Content = resp.Content
		final.Usage.PromptTokens += resp.Usage.PromptTokens
		final.Usage.CompletionTokens += resp.Usage.CompletionTokens
		final.Usage.TotalTokens += resp.Usage.TotalTokens
		final.FinishReason = resp.FinishReason
	}
	return final, nil
}

func (g *Gateway) call(ctx context.Context, target route, request *provider.Request, emit func(string) error, canRetry func() bool) (*provider.Response, error) {
	if len(target.prov.Keys) == 0 {
		return nil, fmt.Errorf("provider %s has no API keys", target.prov.Name)
	}
	now := time.Now()
	if g.circuitOpen(target.prov.Name, now) {
		return nil, &provider.APIError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    fmt.Sprintf("provider %s is paused after repeated failures", target.prov.Name),
		}
	}
	resp, err := g.callKeys(ctx, target, request, emit, canRetry)
	if ctx.Err() == nil {
		g.recordOutcome(target.prov.Name, err, now)
	}
	return resp, err
}

func (g *Gateway) callKeys(ctx context.Context, target route, request *provider.Request, emit func(string) error, canRetry func() bool) (*provider.Response, error) {

	callCtx, cancel := context.WithTimeout(ctx, upstreamWait)
	defer cancel()

	start := g.nextKey(target.prov.Name)

	var lastErr error
	for attempt := 0; attempt < keyAttempts; attempt++ {
		if attempt > 0 && !canRetry() {
			break
		}
		if attempt > 0 {
			select {
			case <-callCtx.Done():
				return nil, callCtx.Err()
			case <-time.After(time.Duration(attempt) * 750 * time.Millisecond):
			}
		}
		key := target.prov.Keys[(start+attempt)%len(target.prov.Keys)]
		var (
			resp *provider.Response
			err  error
		)
		if request.Stream {
			resp, err = target.adapter.Stream(callCtx, request, target.prov, key, emit)
		} else {
			resp, err = target.adapter.Complete(callCtx, request, target.prov, key)
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !provider.Retryable(err) || callCtx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (g *Gateway) runTool(ctx context.Context, call provider.ToolCall) string {
	tool, ok := g.tools[call.Name]
	if !ok {
		return toolError(fmt.Sprintf("unknown tool %q", call.Name))
	}
	result, err := tool.Run(ctx, call.Arguments)
	if err != nil {
		return toolError(err.Error())
	}
	return truncate(result, 32*1024)
}

// isToolError reports whether tool output is a toolError envelope (a failed
// execution), as opposed to genuine command output.
func isToolError(output string) bool {
	trimmed := strings.TrimSpace(output)
	return strings.HasPrefix(trimmed, `{"error":`) || strings.HasPrefix(trimmed, `{"error" :`)
}

func toolError(message string) string {
	encoded, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return `{"error":"tool failed"}`
	}
	return string(encoded)
}

func (g *Gateway) selectTools(names *[]string) ([]provider.ToolDef, error) {
	if names == nil {
		all := make([]provider.ToolDef, 0, len(g.tools))
		for _, tool := range g.tools {
			all = append(all, tool.Def)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		return all, nil
	}

	selected := make([]provider.ToolDef, 0, len(*names))
	for _, name := range *names {
		tool, ok := g.tools[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		selected = append(selected, tool.Def)
	}
	return selected, nil
}

// persistUserDelta records the user's new turns when the upstream call failed,
// so a failed send still leaves its question in history instead of vanishing.
// A question already recorded by an earlier failed attempt is not stacked
// again: repeats reuse the recorded row.
func (g *Gateway) persistUserBefore(stored *session.Session, history, delta []provider.Message) {
	if stored == nil || len(delta) == 0 {
		return
	}
	var recorded []provider.Message
	if len(history) >= len(delta) {
		recorded = history[:len(history)-len(delta)]
	} else {
		recorded = history
	}
	if isRepeatOfTail(recorded, delta) {
		return
	}
	turns := make([]session.TurnInput, 0, len(delta))
	for _, message := range delta {
		if message.Role != "user" {
			continue
		}
		turns = append(turns, session.TurnInput{Role: message.Role, Text: message.Content})
	}
	if len(turns) == 0 {
		return
	}
	if err := g.sessions.AppendTurns(stored.ID, turns); err != nil {
		log.Printf("gateway: save failed delta: %v", err)
	}
}

// isRepeatOfTail reports whether delta is a single user message identical to
// the last visible recorded turn (which then necessarily has no answer yet).
func isRepeatOfTail(recorded, delta []provider.Message) bool {
	if len(delta) != 1 || delta[0].Role != "user" {
		return false
	}
	want := strings.TrimSpace(delta[0].Content)
	if want == "" {
		return false
	}
	for i := len(recorded) - 1; i >= 0; i-- {
		if session.IsToolMessage(recorded[i]) {
			continue
		}
		return recorded[i].Role == "user" && strings.TrimSpace(recorded[i].Content) == want
	}
	return false
}

func (g *Gateway) persist(stored *session.Session, delta []provider.Message, resp *provider.Response, options completionOptions, duration int64) {
	if stored == nil {
		return
	}

	// The incoming question has already been persisted before upstream was
	// called, so here we only append the assistant reply (and the tool trail
	// that belongs to it). Appending it again would duplicate it.
	turns := make([]session.TurnInput, 0, 1+len(resp.Trail)) //+1
	title := ""
	for _, message := range delta {
		if title == "" && message.Role == "user" && strings.TrimSpace(message.Content) != "" {
			title = truncate(strings.TrimSpace(message.Content), 60)
		}
	}
	turns = append(turns, session.TrailTurns(resp.Trail)...)
	generation := resp.GenerationMS
	if generation <= 0 || generation > duration {
		// Either the provider reported no time of its own, or it reported more
		// than the turn took (clock skew between here and upstream). Either way
		// the wall clock is the honest fallback.
		generation = duration
	}
	turns = append(turns, session.TurnInput{
		Role:               "assistant",
		Text:               resp.Content,
		Model:              resp.Model,
		InputTokens:        resp.Usage.PromptTokens,
		OutputTokens:       resp.Usage.CompletionTokens,
		CacheReadTokens:    resp.Usage.CacheReadTokens,
		CacheWriteTokens:   resp.Usage.CacheWriteTokens,
		ReasoningTokens:    resp.Usage.ReasoningTokens,
		InputIncludesCache: resp.Usage.InputIncludesCache,
		DurationMS:         duration,
		GenerationMS:       generation,
	})

	if err := g.sessions.AppendTurns(stored.ID, turns); err != nil {
		log.Printf("gateway: save turns: %v", err)
	}

	saved := session.Session{Provider: resp.Provider, Model: resp.Model}
	if stored.Title == "" {
		saved.Title = title
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := g.sessions.SaveConfig(stored.ID, saved); err == nil {
			break
		} else if attempt == 2 {
			log.Printf("gateway: save session config: %v", err)
		}
	}
}

// mergeHistory matches the client's messages against the stored conversation.
// Clients only ever see visible text, so matching skips stored tool steps; the
// steps that sit between matched messages are kept in full.
func mergeHistory(stored, incoming []provider.Message) (full, delta []provider.Message) {
	visible := make([]int, 0, len(stored))
	for i, message := range stored {
		if !session.IsToolMessage(message) {
			visible = append(visible, i)
		}
	}

	common := 0
	for common < len(visible) && common < len(incoming) {
		match := stored[visible[common]]
		if match.Role != incoming[common].Role || match.Content != incoming[common].Content {
			break
		}
		common++
	}

	switch {
	case common == len(visible):
		full = append(append([]provider.Message(nil), stored...), incoming[common:]...)
		return full, incoming[common:]
	case common == len(incoming):
		return stored, nil
	default:
		cut := 0
		if common > 0 {
			cut = visible[common-1] + 1
		}
		full = append(append([]provider.Message(nil), stored[:cut]...), incoming[common:]...)
		return full, incoming[common:]
	}
}

// appendDelta keeps the stored conversation and adds only what is new.
//
// It exists because mergeHistory matches a common prefix, which is right for a
// client that resends its whole transcript but wrong for one that sends a single
// new turn: stored[0] and the new message never agree, so the default branch
// cut at the first mismatch and every earlier turn was dropped. The model then
// answered each question as if it were the first of the conversation.
func appendDelta(stored, incoming []provider.Message) (full, delta []provider.Message) {
	full = append([]provider.Message(nil), stored...)
	// A retry after a failed attempt reuses the question that was already
	// recorded, instead of stacking a twin the model would see twice.
	if n := len(stored); n > 0 && len(incoming) == 1 &&
		!session.IsToolMessage(stored[n-1]) &&
		stored[n-1].Role == incoming[0].Role &&
		stored[n-1].Content == incoming[0].Content {
		return full, nil
	}
	full = append(full, incoming...)
	return full, incoming
}

// generationMillis is how long the model spent writing, which is the window a
// tokens-per-second rate is computed from. It is deliberately not the duration
// shown to the reader: the wall clock also counts the queue before the first
// token and every tool that ran in between, so a rate built on it read as
// "1t/s" no matter how fast the model actually wrote. A turn that never
// reached a provider falls back to the wall time so it is never reported as
// "0s".
func generationMillis(started time.Time, resp *provider.Response) int64 {
	if resp.GenerationMS > 0 {
		return resp.GenerationMS
	}
	return time.Since(started).Milliseconds()
}

func toInternalProvider(cfg config.Provider) provider.Provider {
	return provider.Provider{
		Name:     cfg.Name,
		Endpoint: cfg.APIURL,
		Keys:     cfg.Keys,
		Adapter:  cfg.Adapter,
		Free:     cfg.Free,
	}
}

func trimProviderPrefix(model, provider string) string {
	if len(model) > len(provider)+1 && strings.EqualFold(model[:len(provider)+1], provider+"/") {
		return model[len(provider)+1:]
	}
	return model
}

func providerNames() string {
	providers := config.GetProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func storedMaxTokens(stored *session.Session) int {
	if stored == nil {
		return 0
	}
	return stored.MaxTokens
}

func storedTemperature(stored *session.Session) *float64 {
	if stored == nil {
		return nil
	}
	return stored.Temperature
}

func storedTopP(stored *session.Session) *float64 {
	if stored == nil {
		return nil
	}
	return stored.TopP
}

func firstNonZero(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

func firstNonZeroPtr(value, fallback *float64) *float64 {
	if value != nil {
		return value
	}
	return fallback
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	errType := "upstream_error"
	if api, ok := err.(*provider.APIError); ok {
		writeError(w, status, errType, api.Error())
		return
	}
	writeError(w, status, errType, err.Error())
}

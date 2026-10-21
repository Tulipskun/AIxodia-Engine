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

	"aixodia-engine/config"
	"aixodia-engine/provider"
	"aixodia-engine/session"
	"aixodia-engine/tools"
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
	// sessionID is the chat's own id. It names the conversation to the
	// provider's prompt cache, so every turn of one chat carries the same
	// cache key and the provider can reuse the prefix it already read.
	sessionID string
	emit      func(string) error
	// Live tool activity, reported around each execution. Nil when nobody
	// watches (non-streaming API); the streaming API and the phone socket
	// forward them so tool use is visible while it runs.
	onToolCall   func(call provider.ToolCall)
	onToolResult func(call provider.ToolCall, text string, durationMs int64, isError bool)
	// onRequest announces one provider request round: waiting when the round
	// starts, done when it ends. The phone used to invent these blocks from
	// send timing, which is why every request wore a "Sending…" it never got;
	// the loop that actually spends the rounds owns them now.
	onRequest func(round int, state string)
	// onUsage reports the turn's running token total after every provider round.
	// A turn that runs tools makes several calls, and the phone shows the
	// session's cost only when the whole turn lands; this is what lets it climb
	// with each round instead of jumping once at the end. The figures are the
	// sum so far, not the round's own, so a watcher that misses one frame still
	// shows the right total on the next.
	onUsage func(usage provider.Usage)
	// onTrail is handed the whole trail so far after every tool round, and trail
	// is where those steps get stored. Both are nil when nothing is persisted.
	onTrail     func(trail []provider.Message)
	trail       *trailWriter
	onStored    func([]session.Turn, error)
	onReasoning func(string) error
	reusedUser  *session.Turn
	// window is what the request is measured against. The provider's own count
	// anchors it when the stored history still carries one, so only the messages
	// added since that measurement have to be estimated.
	window session.Window
}

// trailWriter stores tool steps as each round finishes instead of once the
// whole reply is done. A turn that fails or is cut off after a tool already
// ran (a shell command, say) would otherwise leave no record of it: the trail
// only exists in a response that never arrives.
type trailWriter struct {
	g          *Gateway
	id         string
	saved      int
	onStored   func([]session.Turn, error)
	pending    []session.TurnInput
	pendingEnd int
	// durations is how long each tool call ran, keyed by call id. The trail has
	// no room for it — a tool result is a string — so it is stamped onto the
	// stored row here, where the phone can still read it after the turn closes.
	durations map[string]int64
}

func (g *Gateway) newTrailWriter(stored *session.Session) *trailWriter {
	if stored == nil {
		return nil
	}
	return &trailWriter{g: g, id: stored.ID, durations: map[string]int64{}}
}

// recordDuration remembers how long a call ran so the next flush can stamp it
// on the tool_result row it stores.
func (t *trailWriter) recordDuration(callID string, ms int64) {
	if t == nil || callID == "" {
		return
	}
	if t.durations == nil {
		t.durations = map[string]int64{}
	}
	t.durations[callID] = ms
}

// attachToolDurations writes each stored tool_result row the figure of the call
// it answers. Without it the time is a live-only flicker: the phone shows it
// while the tool runs and loses it the moment the stored rows replace the live
// thread.
func attachToolDurations(rows []session.TurnInput, durations map[string]int64) {
	if len(durations) == 0 {
		return
	}
	for i := range rows {
		if rows[i].Role != "tool_result" {
			continue
		}
		var row struct {
			ToolCallID string `json:"tool_call_id"`
		}
		if json.Unmarshal([]byte(rows[i].Text), &row) != nil {
			continue
		}
		if ms, ok := durations[row.ToolCallID]; ok {
			rows[i].DurationMS = ms
			// The stored tool block carries the same figure, so the row
			// draws settled even where the reader never sees the columns.
			if b, ok := session.ParseBlock(rows[i].Block); ok && b.Type == session.BlockTool {
				b.DurationMs = ms
				rows[i].Block = b.JSON()
			}
		}
	}
}

// flush stores the steps of trail that are not stored yet. A failed write
// leaves them unmarked, so the next round, or persist at the end, retries.
func (t *trailWriter) flush(trail []provider.Message) {
	if t == nil || t.saved >= len(trail) {
		return
	}
	if t.pending == nil {
		t.pending = session.TrailTurns(trail[t.saved:])
		t.pendingEnd = len(trail)
		attachToolDurations(t.pending, t.durations)
	}
	if len(t.pending) > 0 {
		rows, err := t.g.sessions.AppendTurns(t.id, t.pending)
		t.pending = t.pending[len(rows):]
		if t.onStored != nil {
			t.onStored(rows, err)
		}
		if err != nil {
			log.Printf("gateway: save tool steps: %v", err)
			return
		}
	}
	t.saved = t.pendingEnd
	t.pending = nil
	if t.saved < len(trail) {
		t.flush(trail)
	}
}

// rest is the part of trail that flush has not stored.
func (t *trailWriter) rest(trail []provider.Message) []provider.Message {
	if t == nil || t.saved > len(trail) {
		return trail
	}
	return trail[t.saved:]
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
	var savedTurns []session.Turn
	var saved []provider.Message
	if stored != nil {
		var err error
		savedTurns, err = g.sessions.Turns(stored.ID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "database_error", err.Error())
			return
		}
		saved = session.MessagesFromTurns(savedTurns)
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
		sessionID:   sessionIDOf(stored),
		window:      historyWindow(savedTurns, saved, history),
	}
	if len(delta) == 0 && len(incoming) == 1 && incoming[0].Role == "user" {
		for i := len(savedTurns) - 1; i >= 0; i-- {
			if savedTurns[i].Role == "user" && savedTurns[i].Text == incoming[0].Content {
				options.reusedUser = &savedTurns[i]
				break
			}
		}
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
	g.persistUserBefore(stored, history, delta, nil)
	options.trail = g.newTrailWriter(stored)
	options.onTrail = options.trail.flush
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
	turnsSynced := stored != nil
	options.onStored = func(rows []session.Turn, err error) {
		if err != nil {
			turnsSynced = false
		}
		if len(rows) > 0 {
			_ = writeSSE(w, flusher, map[string]any{"turns": rows})
		}
	}
	if options.reusedUser != nil {
		options.onStored([]session.Turn{*options.reusedUser}, nil)
	}
	g.persistUserBefore(stored, history, delta, options.onStored)
	options.trail = g.newTrailWriter(stored)
	if options.trail != nil {
		options.trail.onStored = options.onStored
	}
	options.onTrail = options.trail.flush
	// openRound is the round the loop is spending now. Reasoning and tool
	// events carry no round of their own — they fire inside the provider call
	// — so the handler stamps the round that is open when each one arrives.
	// Order on the stream is the guarantee: a request/waiting always opens
	// the round before its reasoning and calls follow.
	openRound := 0
	options.onRequest = func(round int, state string) {
		openRound = round
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "request", "round": round, "state": state,
			"block": session.RequestBlock(round, state),
		}})
	}
	// The running turn total, one frame per provider round. It carries no block:
	// there is nothing to draw in the thread for a number the strip already
	// owns, so the phone reads it by stage and leaves the thread alone.
	options.onUsage = func(usage provider.Usage) {
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "usage", "round": openRound,
			"prompt_tokens":        usage.PromptTokens,
			"completion_tokens":    usage.CompletionTokens,
			"total_tokens":         usage.TotalTokens,
			"cache_read_tokens":    usage.CacheReadTokens,
			"cache_write_tokens":   usage.CacheWriteTokens,
			"reasoning_tokens":     usage.ReasoningTokens,
			"input_includes_cache": usage.InputIncludesCache,
		}})
	}
	options.onToolCall = func(call provider.ToolCall) {
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "tool_call", "name": call.Name, "call_id": call.ID, "arguments": call.Arguments,
			"round": openRound,
			"block": session.ToolBlock(call.ID, call.Name, call.Arguments, "", false, 0),
		}})
	}
	options.onReasoning = func(text string) error {
		return writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "reasoning", "round": openRound, "text": text,
			"block": session.ReasoningBlock(openRound, text),
		}})
	}
	options.onToolResult = func(call provider.ToolCall, text string, durationMs int64, isError bool) {
		options.trail.recordDuration(call.ID, durationMs)
		_ = writeSSE(w, flusher, map[string]any{"tool_event": map[string]any{
			"stage": "tool_result", "name": call.Name, "call_id": call.ID,
			"text": truncate(text, 4096), "is_error": isError, "duration_ms": durationMs,
			"round": openRound,
			"block": session.ToolBlock(call.ID, call.Name, call.Arguments, truncate(text, 4096), isError, durationMs),
		}})
	}
	resp, err := g.complete(r.Context(), primary, fallback, history, options)
	if err != nil {
		payload := map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}}
		_ = writeSSE(w, flusher, payload)
		writeSSEDone(w, flusher)
		return
	}
	answer := g.persist(stored, delta, resp, options, time.Since(started).Milliseconds())

	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	final := map[string]any{
		"turn":         answer,
		"turns_synced": turnsSynced,
		"id":           id,
		"object":       "chat.completion.chunk",
		"created":      created,
		"model":        resp.Model,
		"provider":     resp.Provider,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":        resp.Usage.PromptTokens,
			"completion_tokens":    resp.Usage.CompletionTokens,
			"total_tokens":         resp.Usage.TotalTokens,
			"cache_read_tokens":    resp.Usage.CacheReadTokens,
			"cache_write_tokens":   resp.Usage.CacheWriteTokens,
			"reasoning_tokens":     resp.Usage.ReasoningTokens,
			"input_includes_cache": resp.Usage.InputIncludesCache,
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
	// Goal and plan tools are scoped to the chat in options.sessionID. The
	// context is the only channel the tool signature carries into a run.
	ctx = tools.WithSession(ctx, options.sessionID)
	current := primary
	usedFallback := false

	window := options.window
	if window.Max <= 0 {
		window.Max = session.DefaultMaxTokens
	}

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
	remind := &reminderState{}

	for round := 0; round < maxToolRounds; {
		if options.onRequest != nil {
			options.onRequest(round+1, session.StateWaiting)
		}
		var messages []provider.Message
		var whole bool
		conversation, messages, whole = g.prepareMessages(conversation, window, options.sessionID, remind)
		request := &provider.Request{
			Model:       current.model,
			Messages:    messages,
			MaxTokens:   options.maxTokens,
			Temperature: options.temperature,
			TopP:        options.topP,
			Stream:      options.stream,
			Tools:       options.tools,
			User:        options.user,
			SessionID:   options.sessionID,
			OnReasoning: options.onReasoning,
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
			if options.onRequest != nil {
				options.onRequest(round+1, session.StateDone)
			}
			return nil, err
		}
		round++
		if served == "" {
			served = current.prov.Name
		}
		// The provider counted exactly the prompt it received. When that prompt
		// was the whole conversation, the count describes the head of the
		// conversation and anchors the next round; when it was a trimmed window,
		// it describes messages that are no longer a prefix, so it is dropped
		// rather than asked to size something it never saw.
		switch {
		case !whole:
			window = session.Window{Max: window.Max}
		case resp.Usage.PromptTokens > 0:
			window = session.Window{Max: window.Max, Usage: resp.Usage.PromptTokens, Messages: len(messages)}
		}

		final.ID = resp.ID
		final.Model = current.model
		final.Provider = served
		// The last round's text is the answer. Earlier rounds are narration
		// before a tool call; they stay with that tool step in the trail.
		// Accumulating them here ran every round together with no separator,
		// so one stored answer read "กำลังเปิดอ่าน...ครับเปิดรูปไม่ขึ้น...ครับ".
		final.Content = resp.Content
		final.ReasoningContent = truncate(resp.ReasoningContent, 32*1024)
		addUsage(&final.Usage, resp.Usage)
		if options.onUsage != nil {
			options.onUsage(final.Usage)
		}
		final.FinishReason = resp.FinishReason

		pending = len(resp.ToolCalls) > 0
		if !pending {
			if options.onRequest != nil {
				options.onRequest(round, session.StateDone)
			}
			break
		}

		assistant := provider.Message{
			Role:             "assistant",
			Content:          resp.Content,
			ToolCalls:        resp.ToolCalls,
			ReasoningContent: final.ReasoningContent,
		}
		conversation = append(conversation, assistant)
		final.Trail = append(final.Trail, assistant)
		for _, call := range resp.ToolCalls {
			if options.onToolCall != nil {
				options.onToolCall(call)
			}
			started := time.Now()
			output, images := g.runTool(ctx, call)
			// A successful goal or plan change means the next model round must
			// see the fresh list, in case add/edit/complete/drop changed it.
			if !isToolError(output) && (strings.HasPrefix(call.Name, "goal_") || strings.HasPrefix(call.Name, "plan_")) {
				remind.dirty = true
			}
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
			// A picture the tool returned cannot ride on the tool result: both
			// wire shapes take only a string there. It goes in its own message
			// so the model can see what the tool actually produced instead of a
			// wall of base64 it cannot read. It stays out of the trail, so it is
			// neither stored nor replayed on the next turn.
			if len(images) > 0 {
				conversation = append(conversation, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("The picture the %s tool returned is attached below.", call.Name),
					Images:  images,
				})
			}
		}
		if options.onTrail != nil {
			options.onTrail(final.Trail)
		}
		if options.onRequest != nil {
			options.onRequest(round, session.StateDone)
		}
		final.FinishReason = "tool_calls"
	}

	if pending {
		// The tool budget for this turn is spent. Taking the tools away alone
		// is not enough: the model keeps narrating ("กำลังดู...อยู่") and that
		// narration is stored as the answer, so the session stops in the middle
		// of the work looking like it was about to continue. Saying the budget
		// is spent, in the conversation it is answering, is what turns the last
		// round into a result the reader can act on.
		conversation = append(conversation, provider.Message{
			Role:    "user",
			Content: outOfRounds,
		})
		// The loop above spent all maxToolRounds rounds, so this closing call
		// without tools is the next one. `round` is out of scope here; the
		// count is exact because pending is only true past the last round.
		const extraRound = maxToolRounds + 1
		if options.onRequest != nil {
			options.onRequest(extraRound, session.StateWaiting)
		}
		_, messages, _ := g.prepareMessages(conversation, window, options.sessionID, remind)
		request := &provider.Request{
			Model:       current.model,
			Messages:    messages,
			MaxTokens:   options.maxTokens,
			Temperature: options.temperature,
			TopP:        options.topP,
			Stream:      options.stream,
			User:        options.user,
			SessionID:   options.sessionID,
			OnReasoning: options.onReasoning,
		}
		callStarted := time.Now()
		resp, err := g.call(ctx, current, request, emit, canRetry)
		final.GenerationMS += time.Since(callStarted).Milliseconds()
		if err != nil {
			if options.onRequest != nil {
				options.onRequest(extraRound, session.StateDone)
			}
			return nil, err
		}
		final.Content = resp.Content
		final.ReasoningContent = truncate(resp.ReasoningContent, 32*1024)
		addUsage(&final.Usage, resp.Usage)
		if options.onUsage != nil {
			options.onUsage(final.Usage)
		}
		final.FinishReason = resp.FinishReason
		if options.onRequest != nil {
			options.onRequest(extraRound, session.StateDone)
		}
	}
	return final, nil
}

// addUsage folds one round's usage into the turn's running total. The cache and
// thinking counts are summed like the token counts they come from: a turn that
// ran tools made several calls, and the prompt the cache served on each is part
// of what the turn cost. InputIncludesCache is a property of the provider, not
// of one round, so any round that says the prompt contains its cache half makes
// the whole total say so.
func addUsage(total *provider.Usage, round provider.Usage) {
	total.PromptTokens += round.PromptTokens
	total.CompletionTokens += round.CompletionTokens
	total.TotalTokens += round.TotalTokens
	total.CacheReadTokens += round.CacheReadTokens
	total.CacheWriteTokens += round.CacheWriteTokens
	total.ReasoningTokens += round.ReasoningTokens
	if round.InputIncludesCache {
		total.InputIncludesCache = true
	}
}

// outOfRounds is the last thing the model reads in a turn that ran out of tool
// rounds. It is written for the model and never stored: it is added to the
// request conversation only, so the trail on record is what really happened.
const outOfRounds = "This turn has no tool calls left, so no further tool can run. " +
	"Answer now in the language of the question: say what you actually did and found, " +
	"and, if the task is not finished, list what is still left. " +
	"Do not describe what you are about to look at, and do not ask for another tool."

func (g *Gateway) call(ctx context.Context, target route, request *provider.Request, emit func(string) error, canRetry func() bool) (*provider.Response, error) {
	if len(target.prov.Keys) == 0 && target.prov.Adapter != "opencode" {
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

	// Non-login mode: an opencode provider may carry no keys (free tier).
	// Treat it as a single empty key so the rotation math below still works.
	keys := target.prov.Keys
	if len(keys) == 0 {
		keys = []string{""}
	}
	start := 0
	if len(target.prov.Keys) > 0 {
		start = g.nextKey(target.prov.Name)
	}

	var lastErr error
	attempts := keyAttempts
	if len(target.prov.Keys) == 0 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
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
		key := keys[(start+attempt)%len(keys)]
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

// runTool executes one tool call and returns its text plus any pictures it
// produced. Pictures are lifted out of the text (see extractToolImages) so the
// base64 never reaches the prompt and the model can be shown the image instead.
func (g *Gateway) runTool(ctx context.Context, call provider.ToolCall) (string, []provider.Image) {
	tool, ok := g.tools[call.Name]
	if !ok {
		return toolError(fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	result, err := tool.Run(ctx, call.Arguments)
	if err != nil {
		return toolError(err.Error()), nil
	}
	text, images := extractToolImages(result)
	return truncate(text, 32*1024), images
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
func (g *Gateway) persistUserBefore(stored *session.Session, history, delta []provider.Message, onStored func([]session.Turn, error)) {
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
	rows, err := g.sessions.AppendTurns(stored.ID, turns)
	if onStored != nil {
		onStored(rows, err)
	}
	if err != nil {
		log.Printf("gateway: save failed delta: %v", err)
	}
}

// answerBlock is the closing answer as a display block. Blank when the turn
// answered with tools alone: an empty text block would draw nothing, and the
// row stays on the role-based path where the empty-answer filter removes it.
func answerBlock(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return session.TextBlock("answer", content).JSON()
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

func (g *Gateway) persist(stored *session.Session, delta []provider.Message, resp *provider.Response, options completionOptions, duration int64) session.Turn {
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
	turns = append(turns, session.TrailTurns(options.trail.rest(resp.Trail))...)
	if options.trail != nil && options.trail.pending != nil {
		turns = append([]session.TurnInput(nil), options.trail.pending...)
		turns = append(turns, session.TrailTurns(resp.Trail[options.trail.pendingEnd:])...)
	}
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
		Block:              answerBlock(resp.Content),
		Model:              resp.Model,
		ReasoningContent:   resp.ReasoningContent,
		InputTokens:        resp.Usage.PromptTokens,
		OutputTokens:       resp.Usage.CompletionTokens,
		CacheReadTokens:    resp.Usage.CacheReadTokens,
		CacheWriteTokens:   resp.Usage.CacheWriteTokens,
		ReasoningTokens:    resp.Usage.ReasoningTokens,
		InputIncludesCache: resp.Usage.InputIncludesCache,
		DurationMS:         duration,
		GenerationMS:       generation,
	})

	answer := session.StoredTurn(sessionIDOf(stored), 0, time.Now().Unix(), turns[len(turns)-1])
	if stored == nil {
		return answer
	}
	rows, err := g.sessions.AppendTurns(stored.ID, turns)
	if options.onStored != nil {
		options.onStored(rows, err)
	}
	if len(rows) == len(turns) {
		answer = rows[len(rows)-1]
	}
	if err != nil {
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
	return answer
}

// historyWindow seeds the size of a request from the provider's last
// measurement of this history. The count is used only while the messages it
// covered are still the head of what will be sent: a client that rewrote its
// history has no claim on a count taken for the history it replaced.
func historyWindow(turns []session.Turn, saved, history []provider.Message) session.Window {
	window := session.Window{Max: session.DefaultMaxTokens}
	messages, tokens := session.Anchor(turns)
	if tokens <= 0 || messages <= 0 || messages > len(saved) || messages > len(history) {
		return window
	}
	for i := 0; i < messages; i++ {
		if saved[i].Role != history[i].Role || saved[i].Content != history[i].Content {
			return window
		}
	}
	window.Usage, window.Messages = tokens, messages
	return window
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

// sessionIDOf is the chat's id, or "" when nothing is persisted. An empty id is
// passed on as empty rather than invented: a cache key that is not the
// conversation's own name would key the wrong prefix, and the provider is
// better left to bucket by the prompt itself.
func sessionIDOf(stored *session.Session) string {
	if stored == nil {
		return ""
	}
	return stored.ID
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

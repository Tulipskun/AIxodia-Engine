package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-engine/provider"
)

type Adapter struct {
	// HeaderFunc is called once per HTTP request and its entries are merged
	// into the outbound headers (used by the opencode client contract).
	HeaderFunc func() map[string]string
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "openai" }

type options struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type request struct {
	Model         string         `json:"model"`
	Messages      []message      `json:"messages"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []tool         `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	User          string         `json:"user,omitempty"`
	options
}

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

// respTool is the FLAT function-tool shape the Responses API expects
// (NOT nested under "function" like chat completions). Sending nested
// tools to /v1/responses gets the request rejected.
type respTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (a *Adapter) Complete(ctx context.Context, req *provider.Request, prov provider.Provider, key string) (*provider.Response, error) {
	body, err := json.Marshal(buildRequest(req, false))
	if err != nil {
		return nil, err
	}
	resp, err := a.doRequest(ctx, prov, key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &provider.APIError{StatusCode: resp.StatusCode, Message: provider.ErrorMessage(raw)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}

	out := &provider.Response{
		ID:    parsed.ID,
		Model: parsed.Model,
		Usage: provider.Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
		},
	}
	if len(parsed.Choices) > 0 {
		choice := parsed.Choices[0]
		out.Content = textContent(choice.Message.Content)
		out.FinishReason = choice.FinishReason
		for _, call := range choice.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}
	return out, nil
}

func (a *Adapter) Stream(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	body, err := json.Marshal(buildRequest(req, true))
	if err != nil {
		return nil, err
	}
	resp, err := a.doRequest(ctx, prov, key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &provider.APIError{StatusCode: resp.StatusCode, Message: provider.ErrorMessage(raw)}
	}

	out := &provider.Response{}
	calls := map[int]*provider.ToolCall{}
	var text strings.Builder
	finish := ""

	err = provider.ReadSSE(resp.Body, func(data string) error {
		if data == "[DONE]" {
			return nil
		}
		var part chunk
		if err := json.Unmarshal([]byte(data), &part); err != nil {
			return nil
		}
		if out.ID == "" {
			out.ID = part.ID
		}
		if out.Model == "" {
			out.Model = part.Model
		}
		if part.Usage != nil {
			out.Usage = provider.Usage{
				PromptTokens:     part.Usage.PromptTokens,
				CompletionTokens: part.Usage.CompletionTokens,
				TotalTokens:      part.Usage.TotalTokens,
			}
		}
		for _, choice := range part.Choices {
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Content != "" && emit != nil {
				if err := emit(choice.Delta.Content); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				target := calls[call.Index]
				if target == nil {
					target = &provider.ToolCall{}
					calls[call.Index] = target
				}
				if call.ID != "" {
					target.ID = call.ID
				}
				if call.Function.Name != "" {
					target.Name = call.Function.Name
				}
				if call.Function.Arguments != "" {
					target.Arguments += call.Function.Arguments
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out.Content = text.String()
	out.FinishReason = finish
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		out.ToolCalls = append(out.ToolCalls, *calls[index])
	}
	return out, nil
}

func buildRequest(req *provider.Request, stream bool) request {
	out := request{
		Model:     req.Model,
		Messages:  toMessages(req.Messages),
		Stream:    stream,
		MaxTokens: req.MaxTokens,
		User:      req.User,
		options:   options{Temperature: req.Temperature, TopP: req.TopP},
	}
	for _, def := range req.Tools {
		var t tool
		t.Type = "function"
		t.Function.Name = def.Name
		t.Function.Description = def.Description
		t.Function.Parameters = def.Parameters
		out.Tools = append(out.Tools, t)
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = "auto"
	}
	return out
}

func toMessages(messages []provider.Message) []message {
	out := make([]message, 0, len(messages))
	for _, incoming := range messages {
		switch incoming.Role {
		case "tool":
			out = append(out, message{
				Role:       "tool",
				Content:    incoming.Content,
				ToolCallID: incoming.ToolCallID,
			})
		case "assistant":
			converted := message{Role: "assistant", Content: incoming.Content}
			for _, call := range incoming.ToolCalls {
				var tc toolCall
				tc.ID = call.ID
				tc.Type = "function"
				tc.Function.Name = call.Name
				tc.Function.Arguments = call.Arguments
				converted.ToolCalls = append(converted.ToolCalls, tc)
			}
			out = append(out, converted)
		default:
			out = append(out, message{Role: incoming.Role, Content: incoming.Content})
		}
	}
	return out
}

func textContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

// responsesRequest is the OpenAI Responses API request shape
// (POST {endpoint}/v1/responses), used for models that do not serve the
// chat completions protocol.
type responsesRequest struct {
	Model           string         `json:"model"`
	Input           any            `json:"input"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
	Tools           []respTool     `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	User            string         `json:"user,omitempty"`
	Include         []string       `json:"include,omitempty"`
	Store           *bool          `json:"store,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
}

type responsesResponse struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		CallID    string `json:"call_id"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	Usage *struct {
		PromptTokens     int `json:"input_tokens"`
		CompletionTokens int `json:"output_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// buildResponsesRequest mirrors what reference clients send to /v1/responses:
// full tool definitions, streaming flags, reasoning interest declaration,
// disabled persistence, and a prompt cache key for session affinity.
func buildResponsesRequest(req *provider.Request, stream bool) responsesRequest {
	items := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		// The Responses API has no role=tool message: tool results travel
		// as function_call_output items and model tool calls as
		// function_call items. Sending chat-style tool messages back gets
		// "`input[N]` did not match any supported type".
		if m.Role == "tool" {
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  textContent(m.Content),
			})
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if text := textContent(m.Content); text != "" {
				items = append(items, map[string]any{"role": "assistant", "content": text})
			}
			for _, call := range m.ToolCalls {
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Name,
					"arguments": call.Arguments,
				})
			}
			continue
		}
		items = append(items, map[string]any{"role": m.Role, "content": textContent(m.Content)})
	}
	bodyReq := responsesRequest{Model: req.Model, Input: items, Stream: stream}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		bodyReq.MaxOutputTokens = &n
	} else {
		n := 32000
		bodyReq.MaxOutputTokens = &n
	}
	bodyReq.Include = []string{"reasoning.encrypted_content"}
	storeFalse := false
	bodyReq.Store = &storeFalse
	bodyReq.PromptCacheKey = newCacheKey()
	if req.User != "" {
		bodyReq.User = req.User
	}
	for _, def := range req.Tools {
		bodyReq.Tools = append(bodyReq.Tools, respTool{
			Type:        "function",
			Name:        def.Name,
			Description: def.Description,
			Parameters:  def.Parameters,
		})
	}
	if len(bodyReq.Tools) > 0 {
		bodyReq.ToolChoice = "auto"
	}
	return bodyReq
}

func newCacheKey() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("ai-engine-%d", time.Now().UnixNano())
	}
	const b62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, 16)
	for i := range out {
		out[i] = b62[int(raw[i%len(raw)])%len(b62)]
	}
	return "ai-engine-" + string(out)
}

// parseResponsesOutput folds a Responses API output array into text,
// tool calls, and usage.
func parseResponsesOutput(parsed responsesResponse) (string, []provider.ToolCall, provider.Usage) {
	var sb strings.Builder
	var calls []provider.ToolCall
	var usage provider.Usage
	if parsed.Usage != nil {
		usage = provider.Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
		}
	}
	for _, item := range parsed.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
				}
			}
		case "function_call":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			calls = append(calls, provider.ToolCall{
				ID: id, Name: item.Name, Arguments: item.Arguments,
			})
		}
	}
	return sb.String(), calls, usage
}

// CompleteViaResponses executes req against the /v1/responses endpoint
// (non-streaming). It is a fallback for models that reject chat completions.
func (a *Adapter) CompleteViaResponses(ctx context.Context, req *provider.Request, prov provider.Provider, key string) (*provider.Response, error) {
	endpoint, err := provider.Endpoint(prov.Endpoint, "/v1/responses", "/responses")
	if err != nil {
		return nil, err
	}
	bodyReq := buildResponsesRequest(req, false)
	body, err := json.Marshal(bodyReq)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", provider.UserAgent)
	if a.HeaderFunc != nil {
		for k, v := range a.HeaderFunc() {
			httpRequest.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &provider.APIError{StatusCode: resp.StatusCode, Message: provider.ErrorMessage(raw)}
	}
	var parsed responsesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("openai: decode responses response: %w", err)
	}
	out := &provider.Response{ID: parsed.ID, Model: parsed.Model}
	out.Content, out.ToolCalls, out.Usage = parseResponsesOutput(parsed)
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return nil, fmt.Errorf("openai: empty responses output")
	}
	return out, nil
}

// responsesStreamEvent is a Responses API SSE event (only fields we use).
type responsesStreamEvent struct {
	Type     string             `json:"type"`
	Delta    string             `json:"delta"`
	ItemID   string             `json:"item_id"`
	Response *responsesResponse `json:"response"`
}

// StreamViaResponses executes req against /v1/responses with streaming,
// mirroring what reference clients send (full tools, prompt cache key).
// Text deltas are emitted as they arrive; tool calls are assembled from
// function_call_arguments deltas and finalized from response.completed.
func (a *Adapter) StreamViaResponses(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	endpoint, err := provider.Endpoint(prov.Endpoint, "/v1/responses", "/responses")
	if err != nil {
		return nil, err
	}
	bodyReq := buildResponsesRequest(req, true)
	body, err := json.Marshal(bodyReq)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("User-Agent", provider.UserAgent)
	if a.HeaderFunc != nil {
		for k, v := range a.HeaderFunc() {
			httpRequest.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &provider.APIError{StatusCode: resp.StatusCode, Message: provider.ErrorMessage(raw)}
	}

	out := &provider.Response{}
	var text strings.Builder
	argBufs := map[string]*strings.Builder{}
	callOrder := []string{}
	callNames := map[string]string{}
	var completed *responsesResponse
	finished := false
	cutShort := false

	serr := provider.ReadSSE(resp.Body, func(data string) error {
		if data == "[DONE]" {
			finished = true
			return nil
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
			if emit != nil {
				if err := emit(ev.Delta); err != nil {
					return err
				}
			}
		case "response.function_call_arguments.delta":
			id := ev.ItemID
			if id == "" {
				id = "call_0"
			}
			buf := argBufs[id]
			if buf == nil {
				buf = &strings.Builder{}
				argBufs[id] = buf
				callOrder = append(callOrder, id)
			}
			buf.WriteString(ev.Delta)
		case "response.output_item.added", "response.output_item.done":
			// Item identity may arrive here before arguments finish streaming;
			// names are reconciled again from the completed object below.
		case "response.completed":
			if ev.Response != nil {
				completed = ev.Response
			}
			finished = true
		case "response.incomplete":
			// Token-limit cutoff: keep the partial output instead of
			// failing, mirroring finish_reason=length on chat paths.
			if ev.Response != nil {
				completed = ev.Response
			}
			cutShort = true
			finished = true
		case "response.failed", "error":
			return fmt.Errorf("openai: responses stream failed: %s", data[:min(300, len(data))])
		}
		return nil
	})
	if serr != nil {
		return nil, serr
	}
	if completed != nil {
		content, calls, usage := parseResponsesOutput(*completed)
		out.Content, out.ToolCalls, out.Usage = content, calls, usage
		out.ID, out.Model = completed.ID, completed.Model
		if cutShort {
			out.FinishReason = "length"
		}
	} else {
		out.Content = text.String()
		for _, id := range callOrder {
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{
				ID: id, Name: callNames[id], Arguments: argBufs[id].String(),
			})
		}
	}
	if out.Content == "" && len(out.ToolCalls) == 0 && !finished {
		return nil, fmt.Errorf("openai: empty responses stream")
	}
	return out, nil
}

func (a *Adapter) doRequest(ctx context.Context, prov provider.Provider, key string, body []byte) (*http.Response, error) {
	endpoint, err := provider.Endpoint(prov.Endpoint, "/v1/chat/completions", "/chat/completions")
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", provider.UserAgent)
	if a.HeaderFunc != nil {
		for name, value := range a.HeaderFunc() {
			httpRequest.Header.Set(name, value)
		}
	}
	return provider.HTTP.Do(httpRequest)
}

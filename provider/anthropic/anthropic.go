// Package anthropic implements provider.Adapter for the Anthropic Messages API
// (POST {endpoint}/v1/messages), with tool use and SSE streaming.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aixodia-engine/provider"
)

const (
	apiVersion       = "2023-06-01"
	defaultPath      = "/v1/messages"
	defaultMaxTokens = 4096
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "anthropic" }

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Source    *wireImage      `json:"source,omitempty"`
}

// wireImage is Anthropic's inline picture: base64 with its media type spelled
// out, because the block carries no other hint about how to decode it.
type wireImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      string        `json:"system,omitempty"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// buildRequest converts the provider-neutral conversation into Anthropic's
// shape. System messages move to the top-level field, assistant tool calls
// become tool_use blocks, and tool results become tool_result blocks inside a
// user message. Consecutive tool results share one user message, which is what
// the API expects after a multi-call assistant turn.
func buildRequest(req *provider.Request, stream bool) wireRequest {
	out := wireRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}

	var system []string
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			if strings.TrimSpace(msg.Content) != "" {
				system = append(system, msg.Content)
			}
		case "assistant":
			var blocks []wireBlock
			if msg.Content != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				input := json.RawMessage(call.Arguments)
				if !json.Valid(input) || len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = []wireBlock{{Type: "text", Text: " "}}
			}
			out.Messages = append(out.Messages, wireMessage{Role: "assistant", Content: blocks})
		case "tool":
			block := wireBlock{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: msg.Content}
			if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == "user" && isToolResultMessage(out.Messages[n-1]) {
				out.Messages[n-1].Content = append(out.Messages[n-1].Content, block)
			} else {
				out.Messages = append(out.Messages, wireMessage{Role: "user", Content: []wireBlock{block}})
			}
		default:
			blocks := make([]wireBlock, 0, len(msg.Images)+1)
			if text := msg.Content; strings.TrimSpace(text) != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: text})
			}
			for _, img := range msg.Images {
				media := img.MediaType
				if media == "" {
					media = "image/jpeg"
				}
				blocks = append(blocks, wireBlock{
					Type: "image",
					Source: &wireImage{
						Type:      "base64",
						MediaType: media,
						Data:      base64.StdEncoding.EncodeToString(img.Data),
					},
				})
			}
			if len(blocks) == 0 {
				blocks = []wireBlock{{Type: "text", Text: " "}}
			}
			out.Messages = append(out.Messages, wireMessage{Role: "user", Content: blocks})
		}
	}
	out.System = strings.Join(system, "\n\n")

	for _, tool := range req.Tools {
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, wireTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
	}
	return out
}

func isToolResultMessage(m wireMessage) bool {
	return len(m.Content) > 0 && m.Content[0].Type == "tool_result"
}

type wireResponse struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
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

	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	out := &provider.Response{
		ID:           parsed.ID,
		Model:        parsed.Model,
		FinishReason: finishReason(parsed.StopReason),
		Usage: provider.Usage{
			PromptTokens:     parsed.Usage.InputTokens,
			CompletionTokens: parsed.Usage.OutputTokens,
			TotalTokens:      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		},
	}
	var text strings.Builder
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: argumentsString(block.Input),
			})
		}
	}
	out.Content = text.String()
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return nil, &provider.APIError{StatusCode: resp.StatusCode, Message: provider.ErrorMessage(raw)}
	}

	out := &provider.Response{}
	type block struct {
		kind string
		id   string
		name string
		args strings.Builder
		text strings.Builder
	}
	blocks := map[int]*block{}
	var order []int
	var streamErr error

	err = provider.ReadSSE(resp.Body, func(data string) error {
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil
		}
		switch event.Type {
		case "message_start":
			out.ID = event.Message.ID
			out.Model = event.Message.Model
			out.Usage.PromptTokens = event.Message.Usage.InputTokens
		case "content_block_start":
			b := &block{kind: event.ContentBlock.Type, id: event.ContentBlock.ID, name: event.ContentBlock.Name}
			blocks[event.Index] = b
			order = append(order, event.Index)
		case "content_block_delta":
			b := blocks[event.Index]
			if b == nil {
				return nil
			}
			switch event.Delta.Type {
			case "text_delta":
				b.text.WriteString(event.Delta.Text)
				out.Content += event.Delta.Text
				if emit != nil {
					if err := emit(event.Delta.Text); err != nil {
						return err
					}
				}
			case "input_json_delta":
				b.args.WriteString(event.Delta.PartialJSON)
			}
		case "message_delta":
			out.FinishReason = finishReason(event.Delta.StopReason)
			out.Usage.CompletionTokens = event.Usage.OutputTokens
		case "error":
			streamErr = fmt.Errorf("anthropic stream: %s", event.Error.Message)
			return streamErr
		}
		return nil
	})
	if streamErr != nil {
		return nil, streamErr
	}
	if err != nil {
		return nil, err
	}

	for _, index := range order {
		b := blocks[index]
		if b.kind == "tool_use" {
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{
				ID:        b.id,
				Name:      b.name,
				Arguments: argumentsString(json.RawMessage(b.args.String())),
			})
		}
	}
	out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	return out, nil
}

func (a *Adapter) doRequest(ctx context.Context, prov provider.Provider, key string, body []byte) (*http.Response, error) {
	endpoint, err := provider.Endpoint(prov.Endpoint, defaultPath, "/messages")
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json, text/event-stream")
	httpRequest.Header.Set("User-Agent", provider.UserAgent)
	httpRequest.Header.Set("x-api-key", key)
	httpRequest.Header.Set("anthropic-version", apiVersion)
	return provider.HTTP.Do(httpRequest)
}

func finishReason(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return stop
	}
}

func argumentsString(input json.RawMessage) string {
	if len(input) == 0 || string(input) == "null" {
		return "{}"
	}
	return string(input)
}

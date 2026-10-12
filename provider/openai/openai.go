package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

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

type request struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
	Tools     []tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	User      string    `json:"user,omitempty"`
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

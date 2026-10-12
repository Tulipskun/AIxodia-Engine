package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const UserAgent = "ai-engine/1.0"

var HTTP = &http.Client{}

type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stream      bool
	Tools       []ToolDef
	User        string
}

type Response struct {
	ID           string
	Model        string
	Provider     string
	Content      string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string
	// Trail holds the tool steps taken while producing Content, in order: an
	// assistant message carrying ToolCalls, then one tool message per call.
	Trail []Message
	// GenerationMS is the time spent inside the provider calls, summed over every
	// round. It is not the wall time of the turn: that also covers waiting for
	// the provider and running tools, and dividing the token count by it reported
	// a speed the model never had.
	GenerationMS int64
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

type Provider struct {
	Name     string
	Endpoint string
	Keys     []string
	Adapter  string
	Model    string
	Free     bool
}

type Adapter interface {
	Name() string
	Complete(ctx context.Context, req *Request, prov Provider, key string) (*Response, error)
	Stream(ctx context.Context, req *Request, prov Provider, key string, emit func(string) error) (*Response, error)
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("upstream HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream HTTP %d: %s", e.StatusCode, e.Message)
}

func Retryable(err error) bool {
	api, ok := err.(*APIError)
	if !ok {
		return true
	}
	return api.StatusCode == 401 || api.StatusCode == 408 || api.StatusCode == 429 || api.StatusCode >= 500
}

func Endpoint(endpoint, defaultPath, suffix string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("provider endpoint is empty")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("provider endpoint: %w", err)
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, suffix):
	case path == "":
		parsed.Path = defaultPath
	default:
		parsed.Path = path + suffix
	}
	return parsed.String(), nil
}

func ErrorMessage(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		if payload.Error.Message != "" {
			return payload.Error.Message
		}
		if len(payload.Errors) > 0 && payload.Errors[0].Message != "" {
			return payload.Errors[0].Message
		}
	}
	var flat struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(text), &flat); err == nil && flat.Message != "" {
		return flat.Message
	}
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func ReadSSE(r io.Reader, fn func(data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if data.Len() > 0 {
				if err := fn(data.String()); err != nil {
					return err
				}
				data.Reset()
			}
		}
	}
	if data.Len() > 0 {
		if err := fn(data.String()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

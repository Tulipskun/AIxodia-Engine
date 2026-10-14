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

// Image is one picture carried by a message. Data is the encoded file and
// MediaType is its MIME type ("image/jpeg"); both wire formats need the type to
// decode the bytes, and neither accepts a bare blob.
type Image struct {
	MediaType string
	Data      []byte
}

type Message struct {
	Role string
	// Content is the text of the message. When Images is empty it is what the
	// wire carries; otherwise the text becomes one part of a multi-part body.
	Content    string
	Images     []Image
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
	// ReasoningContent is what a reasoning model wrote before the answer. It is
	// never shown and not needed to read the answer, but providers that serve a
	// reasoning model validate the next request against it: an assistant turn
	// replayed without the field is refused with a missing-parameter 400, so it
	// has to travel with the message.
	ReasoningContent string
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
	Model    string
	Messages []Message
	// SessionID is this engine's session. An adapter that keeps its own
	// conversation — the OpenCode client does — names it so the two stay in step
	// without a mapping table.
	SessionID   string
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
	// ReasoningContent is the reasoning the model wrote before Content. It
	// belongs in the assistant message the turn leaves behind, which is what
	// the next request replays (see Message.ReasoningContent).
	ReasoningContent string
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
	// The rest are reported by some providers and not others. They are kept so
	// the footer can show what a prompt actually cost instead of only its size.
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	// InputIncludesCache says whether PromptTokens already contains the cache
	// read. Providers disagree, and guessing wrong doubles or halves the prompt
	// in the footer, so it is recorded rather than assumed.
	InputIncludesCache bool
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

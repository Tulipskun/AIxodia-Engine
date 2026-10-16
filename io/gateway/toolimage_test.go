package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-engine/config"
	"ai-engine/provider"
	"ai-engine/provider/registry"
	"ai-engine/tools"
)

// tinyPNG is a 2x2 PNG as base64. It is a real picture so the tools that
// inspect one accept it, and it is small enough to read in a failure message.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAC0lEQVR4nGNgQAcAABIAAXfx+gAAAAAASUVORK5CYII="

func TestExtractToolImagesLiftsPicturesOutOfText(t *testing.T) {
	text := "board.png — image/png (2x2)\ndata:image/png;base64," + tinyPNG
	got, images := extractToolImages(text)
	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	if images[0].MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png", images[0].MediaType)
	}
	if len(images[0].Data) == 0 {
		t.Error("decoded picture is empty")
	}
	if strings.Contains(got, tinyPNG) || strings.Contains(got, "base64,") {
		t.Errorf("base64 leaked into the text: %q", got)
	}
	if !strings.Contains(got, "board.png") {
		t.Errorf("the text before the URI was dropped: %q", got)
	}
	if !strings.Contains(got, "[picture attached below: image/png]") {
		t.Errorf("missing marker: %q", got)
	}
}

func TestExtractToolImagesLeavesTextWithoutPictures(t *testing.T) {
	text := "total 4K\n-rw-r--r-- 1 root root 2 Oct 16 note.txt\n"
	got, images := extractToolImages(text)
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if got != text {
		t.Errorf("text changed:\n got %q\nwant %q", got, text)
	}
}

func TestExtractToolImagesIgnoresNonImageDataURIs(t *testing.T) {
	// A PDF data URI names a document, not a picture, and stays as text.
	text := "report.pdf\ndata:application/pdf;base64,JVBERi0xLjQK"
	got, images := extractToolImages(text)
	if len(images) != 0 {
		t.Fatalf("images = %d, want 0", len(images))
	}
	if got != text {
		t.Errorf("text changed:\n got %q\nwant %q", got, text)
	}
}

// TestToolPictureReachesTheModelAsAnImage is the point of the change: a tool
// that reads a picture used to send the model a base64 string it could not see.
// The picture must arrive as an image part and the base64 must not appear as
// tool text.
func TestToolPictureReachesTheModelAsAnImage(t *testing.T) {
	var second []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tool_call_id"`) {
			second = body
			writeFake(w, "done", nil, "stop")
			return
		}
		writeFake(w, "", []string{"shot"}, "tool_calls")
	}))
	t.Cleanup(upstream.Close)

	config.Providers = []config.Provider{{
		ID:      "9",
		Name:    "mock",
		APIURL:  upstream.URL,
		Keys:    []string{"test-key"},
		Adapter: "openai",
	}}
	g := New(newMemStore(), registry.New(), map[string]tools.Tool{
		"shot": {
			Def: provider.ToolDef{Name: "shot"},
			Run: func(context.Context, string) (string, error) {
				return "board.png — image/png (2x2)\ndata:image/png;base64," + tinyPNG, nil
			},
		},
	})
	impl, err := g.registry.Require("openai")
	if err != nil {
		t.Fatal(err)
	}
	primary := route{adapter: impl, prov: toInternalProvider(config.Providers[0]), model: "test-model"}

	resp, err := g.complete(context.Background(), primary, nil,
		[]provider.Message{{Role: "user", Content: "what is on the board"}},
		completionOptions{maxTokens: 50, tools: []provider.ToolDef{{Name: "shot"}}})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Content != "done" {
		t.Errorf("content = %q, want done", resp.Content)
	}

	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(second, &request); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	imageSent := false
	for _, m := range request.Messages {
		if m.Role == "tool" && strings.Contains(string(m.Content), "base64,") {
			t.Errorf("tool result still feeds base64 text to the model: %.120s", m.Content)
		}
		if strings.Contains(string(m.Content), "data:image/png;base64,") {
			imageSent = true
		}
	}
	if !imageSent {
		t.Errorf("the picture the tool returned never reached the model as an image:\n%s", second)
	}
}

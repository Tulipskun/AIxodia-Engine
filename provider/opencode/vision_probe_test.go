package opencode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"ai-engine/provider"
	"ai-engine/tools"
)

// TestVisionProbe asks the live endpoint whether a model can see a picture at
// all. It is skipped unless a key is supplied, because it spends real credit and
// talks to the network: the answer is a property of the upstream service, not of
// this code, so it cannot be asserted from a fixture.
//
//	AIXODIA_PROBE_KEY=... AIXODIA_PROBE_IMAGE=probe.jpg \
//	  go test ./provider/opencode/ -run TestVisionProbe -v
func TestVisionProbe(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("AIXODIA_PROBE_KEY"))
	imagePath := strings.TrimSpace(os.Getenv("AIXODIA_PROBE_IMAGE"))
	if key == "" || imagePath == "" {
		t.Skip("set AIXODIA_PROBE_KEY and AIXODIA_PROBE_IMAGE to probe the live endpoint")
	}
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("AIXODIA_PROBE_MODEL")
	if model == "" {
		model = "muse-spark-1.3-contributor-free"
	}

	// The free-tier gate only passes the full rich tool declarations, so the
	// probe has to look like a real turn and not a bare curl.
	builtin := tools.Builtin()
	defs := make([]provider.ToolDef, 0, len(builtin))
	for _, tool := range builtin {
		defs = append(defs, tool.Def)
	}

	// A control run in the same conditions, so a refusal can be told apart from
	// a model that simply cannot see: the same request without the picture.
	question := "What shape is in this image and what colour is it? One short sentence."
	var images []provider.Image
	if os.Getenv("AIXODIA_PROBE_NO_IMAGE") == "" {
		images = []provider.Image{{MediaType: "image/jpeg", Data: raw}}
	} else {
		question = "Reply with the single word: ready"
	}
	req := &provider.Request{
		Model: model,
		// Left unset on purpose: the engine sends no max_tokens unless the chat
		// pinned one, and sending a small value here made every attempt fail in
		// a way that had nothing to do with the picture.
		MaxTokens: 0,
		Tools:     defs,
		Messages: []provider.Message{{
			Role:    "user",
			Content: question,
			Images:  images,
		}},
	}
	// The catalogue gives each model its own base URL, and it is not the one the
	// provider row carries: muse-spark is served from inference/openai/v1 while
	// the row points at zen/v1, which answers "Model does not support this
	// protocol" for it.
	endpoint := os.Getenv("AIXODIA_PROBE_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://opencode.ai/inference/openai/v1"
	}
	prov := provider.Provider{Name: "Opencode", Endpoint: endpoint, Keys: []string{key}}

	// Capture what actually goes on the wire. Without this the probe is not a
	// usable instrument: a refusal cannot be told apart from a request that
	// simply differs from the one the engine sends.
	if os.Getenv("AIXODIA_PROBE_DUMP") != "" {
		provider.HTTP.Transport = dumpTransport{inner: provider.HTTP.Transport}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adapter := New()
	var streamed strings.Builder
	resp, err := adapter.Stream(ctx, req, prov, key, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("vision request failed: %v", err)
	}
	answer := resp.Content
	if answer == "" {
		answer = streamed.String()
	}
	fmt.Printf("\n=== model: %s\n=== image: %s (%d bytes, base64 %d)\n=== answer: %s\n",
		model, imagePath, len(raw), len(base64.StdEncoding.EncodeToString(raw)), answer)
	if strings.TrimSpace(answer) == "" {
		t.Error("the endpoint accepted the image but answered nothing")
	}
}

// dumpTransport prints each outgoing request body, so a probe failure can be
// compared against the request the engine actually sends.
type dumpTransport struct{ inner http.RoundTripper }

func (d dumpTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			fmt.Printf("\n--- %s %s\n%s\n", r.Method, r.URL.Path, pretty.String())
		} else {
			fmt.Printf("\n--- %s %s (%d bytes)\n", r.Method, r.URL.Path, len(raw))
		}
	}
	if d.inner == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return d.inner.RoundTrip(r)
}

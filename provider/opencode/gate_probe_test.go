package opencode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"ai-engine/provider"
	"ai-engine/tools"
)

// TestGateProbe asks the live endpoint whether this caller is accepted.
//
// It is skipped unless a key is supplied, because the answer is a property of
// the endpoint and not of this code: the point is to find out whether matching
// the client's connection is enough, and that can only be measured against the
// endpoint itself.
//
//	AIXODIA_PROBE_KEY=... go test ./provider/opencode/ -run TestGateProbe -v
func TestGateProbe(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("AIXODIA_PROBE_KEY"))
	if key == "" {
		t.Skip("set AIXODIA_PROBE_KEY to probe the live endpoint")
	}
	endpoint := strings.TrimSpace(os.Getenv("AIXODIA_PROBE_ENDPOINT"))
	if endpoint == "" {
		endpoint = "https://opencode.ai/zen/v1"
	}
	builtin := tools.Builtin()
	defs := make([]provider.ToolDef, 0, len(builtin))
	for _, tool := range builtin {
		defs = append(defs, tool.Def)
	}
	prov := provider.Provider{Name: "Opencode", Endpoint: endpoint, Keys: []string{key}}

	// The user agent is varied because it is the one part of the request the
	// engine had wrong, and there is no point matching the connection if the
	// name on it is still refused.
	agents := []string{
		clientID,
		"opencode/latest/2.0.27/cli",
	}
	for _, agent := range agents {
		t.Run(strings.ReplaceAll(agent, "/", "_"), func(t *testing.T) {
			adapter := New()
			adapter.agent = agent
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			resp, err := adapter.Complete(ctx, &provider.Request{
				Model:     "muse-spark-1.3-contributor-free",
				Tools:     defs,
				MaxTokens: 0,
				Messages:  []provider.Message{{Role: "user", Content: "say ready"}},
			}, prov, key)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if strings.TrimSpace(resp.Content) == "" {
				t.Error("accepted but answered nothing")
			}
			t.Logf("accepted: %q", resp.Content)
		})
	}
}

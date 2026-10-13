package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"ai-engine/tools"
)

// TestRawRequestProbe sends the request written by hand, in the captured header
// order, over the connection negotiated with the recorded ClientHello.
func TestRawRequestProbe(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("AIXODIA_PROBE_KEY"))
	if key == "" {
		t.Skip("set AIXODIA_PROBE_KEY")
	}
	builtin := tools.Builtin()
	_ = builtin
	body, _ := json.Marshal(map[string]any{
		"model":    "muse-spark-1.3-contributor-free",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "say ready"}},
	})
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Connection", "keep-alive")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", clientAgentMeasured)
	headers.Set("x-opencode-client", "cli")
	headers.Set("x-opencode-org-id", "wrk_01KWH4NZA492CFGV4K9DCMMSH4")
	headers.Set("x-opencode-project", "3c45a4f74df4542fa0b6ba0c03c922b76e650f0f")
	session := "ses_" + strings.Repeat("a", 12) + strings.Repeat("b", 14)
	headers.Set("x-opencode-session", session)
	headers.Set("x-opencode-session-id", session)
	headers.Set("x-session-affinity", session)
	headers.Set("x-session-id", session)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, payload, err := bunRequest(ctx, "https://opencode.ai/zen/v1/chat/completions", http.MethodPost, headers, body, 90*time.Second)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	t.Logf("status %d, %d bytes", resp.StatusCode, len(payload))
	t.Logf("body: %.300s", string(payload))
	if resp.StatusCode == 200 {
		t.Log("ACCEPTED")
	}
}

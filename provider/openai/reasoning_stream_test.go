package openai

import (
	"aixodia-engine/provider"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponsesReasoningIsStreamedAndStored(t *testing.T) {
	for _, kind := range []string{"response.reasoning_summary_text.delta", "response.reasoning_text.delta"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: {\"type\":%q,\"delta\":\"real reasoning\"}\n\n", kind)
				fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`+"\n\n")
			}))
			defer srv.Close()
			var got string
			req := &provider.Request{Model: "m", OnReasoning: func(s string) error { got += s; return nil }}
			resp, err := (&Adapter{}).StreamViaResponses(context.Background(), req, provider.Provider{Endpoint: srv.URL}, "key", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != "real reasoning" || resp.ReasoningContent != got || resp.Content != "ok" {
				t.Fatalf("callback=%q response=%+v", got, resp)
			}
		})
	}
}

func TestResponsesNeverDisplaysEncryptedReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"output":[{"type":"reasoning","encrypted_content":"secret ciphertext","summary":[{"type":"summary_text","text":"readable summary"}]},{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`+"\n\n")
	}))
	defer srv.Close()
	var got string
	req := &provider.Request{Model: "m", OnReasoning: func(s string) error { got += s; return nil }}
	resp, err := (&Adapter{}).StreamViaResponses(context.Background(), req, provider.Provider{Endpoint: srv.URL}, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "readable summary" || resp.ReasoningContent != got {
		t.Fatalf("callback=%q response=%+v", got, resp)
	}
}

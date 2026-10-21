package session

import "testing"

// The phone draws the session strip from metadata, not from the page of rows it
// holds. These are the fields sessionSelect sums and sessionFromRow hands to
// that metadata; a wrong tag here silently empties the strip even though the
// query itself is right.
func TestSessionFromRowCarriesTheSummedUsage(t *testing.T) {
	row := map[string]any{
		"id": "s1", "title": "chat", "provider": "p", "model": "m",
		"created_at": float64(1), "updated_at": float64(2),
		"usage_prompt_tokens": float64(165),
		"usage_cache_read":    float64(90),
		"usage_fresh_input":   float64(75),
		"usage_output_tokens": float64(15),
		"usage_total_ms":      float64(1500),
		"usage_last_turn_ms":  float64(500),
	}
	got := sessionFromRow(row)
	if got.Usage == nil {
		t.Fatal("usage is nil")
	}
	want := SessionUsage{
		PromptTokens: 165, CacheRead: 90, FreshInput: 75,
		OutputTokens: 15, TotalMS: 1500, LastTurnMS: 500,
	}
	if *got.Usage != want {
		t.Fatalf("usage = %+v, want %+v", *got.Usage, want)
	}
	if got.Usage.FreshInput+got.Usage.CacheRead != got.Usage.PromptTokens {
		t.Fatalf("prompt split does not add up: %+v", *got.Usage)
	}
}

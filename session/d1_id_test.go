package session

import "testing"

// The phone keys messages by the D1 row id, so it has to survive the row
// mapping as a number. D1's HTTP API reports integers as float64.
func TestTurnFromRowCarriesSequentialMessageID(t *testing.T) {
	turn := turnFromRow(map[string]any{
		"id":         float64(42),
		"session_id": "s1",
		"seq":        float64(3),
		"role":       "user",
		"text":       "hi",
	})
	if turn.ID != 42 {
		t.Fatalf("turn.ID = %d, want 42", turn.ID)
	}
}

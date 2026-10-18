package session

import (
	"errors"
	"path/filepath"
	"testing"

	"aixodia-engine/localdb"
)

func testSQLite(t *testing.T) *SQLite {
	t.Helper()
	handle, err := localdb.Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	return NewSQLite(handle)
}

// The agent route pins a model for one chat. It must not touch anything else:
// the chat path saves provider/model/title when a reply succeeds, and a pin
// that cleared the title would rename a chat on the phone.
func TestSaveAgentChangesOnlyWhatItIsGiven(t *testing.T) {
	st := testSQLite(t)
	created, err := st.Create(NewSession{Title: "ของเดิม", Provider: "Opencode", Model: "big-pickle"})
	if err != nil {
		t.Fatal(err)
	}

	provider, model, subModel := "Other", "other-model", "small"
	enabled := true
	if err := st.SaveAgent(created.ID, AgentPatch{
		Provider: &provider, Model: &model, SubModel: &subModel, SubEnabled: &enabled,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "Other" || got.Model != "other-model" || got.SubModel != "small" || !got.SubEnabled {
		t.Fatalf("session = %+v, want the pinned values", got)
	}
	if got.Title != "ของเดิม" || got.CreatedAt != created.CreatedAt {
		t.Fatalf("session = %+v, want the title and created_at untouched", got)
	}
	if got.SubProvider != "" {
		t.Fatalf("sub_provider = %q, want it untouched", got.SubProvider)
	}
}

// The route answers 404 for a chat that is not here, so the store has to say
// which failure that is.
func TestSaveAgentOfAMissingSession(t *testing.T) {
	st := testSQLite(t)
	model := "m"
	err := st.SaveAgent("no-such-session", AgentPatch{Model: &model})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveAgent error = %v, want ErrNotFound", err)
	}
	if _, err := st.Get("no-such-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

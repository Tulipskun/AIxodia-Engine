package session

import (
	"fmt"
	"strings"
	"time"
)

// AgentPatch is the part of a chat's agent choice a PATCH changes. A nil field
// keeps what is stored: the phone sends only the menu entries it touched, and a
// field it left out is not a request to clear it.
type AgentPatch struct {
	Provider    *string
	Model       *string
	SubProvider *string
	SubModel    *string
	SubEnabled  *bool
}

// Empty reports whether the patch asks for nothing, which is what an empty body
// means.
func (p AgentPatch) Empty() bool {
	return p.Provider == nil && p.Model == nil && p.SubProvider == nil && p.SubModel == nil && p.SubEnabled == nil
}

// SaveAgent writes a chat's pinned agent choice and moves updated_at with it:
// the chat list is ordered by that column, and pinning a model is a change to
// the chat. The title and the model the chat itself runs on are left alone —
// the chat path writes those when a reply succeeds, and a pin that blanked them
// would rename the chat on the phone.
func (s *SQLite) SaveAgent(id string, patch AgentPatch) error {
	columns := []string{}
	values := []any{}
	add := func(column string, value any) {
		columns = append(columns, column+"=?")
		values = append(values, value)
	}
	if patch.Provider != nil {
		add("provider", *patch.Provider)
	}
	if patch.Model != nil {
		add("model", *patch.Model)
	}
	if patch.SubProvider != nil {
		add("sub_provider", *patch.SubProvider)
	}
	if patch.SubModel != nil {
		add("sub_model", *patch.SubModel)
	}
	if patch.SubEnabled != nil {
		add("sub_enabled", boolInt(*patch.SubEnabled))
	}
	if len(columns) == 0 {
		return nil
	}
	add("updated_at", time.Now().Unix())
	values = append(values, id)

	result, err := s.db.Exec(`UPDATE chat_sessions SET `+strings.Join(columns, ", ")+` WHERE id=?`, values...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return nil
}

package session

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

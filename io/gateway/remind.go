package gateway

import (
	"log"
	"strings"

	"aixodia-engine/goals"
	"aixodia-engine/provider"
	"aixodia-engine/session"
)

// The reminder is a transient user message. It is never stored in chat history
// or in the turn trail; it exists only in the request conversation, so the
// model sees the current lists without the phone showing an extra user bubble.
const (
	reminderOpen  = "<system-remind>"
	reminderClose = "</system-remind>"
)

// reminderState is per-turn bookkeeping. dirty is set when a goal or plan tool
// succeeded; checkedEmpty remembers that this turn already looked at an empty
// store, so a long tool loop does not query D1 every round for nothing.
type reminderState struct {
	dirty        bool
	checkedEmpty bool
}

func isReminder(message provider.Message) bool {
	return message.Role == "user" &&
		strings.HasPrefix(strings.TrimSpace(message.Content), reminderOpen)
}

func hasReminder(messages []provider.Message) bool {
	for _, message := range messages {
		if isReminder(message) {
			return true
		}
	}
	return false
}

// stripReminders removes every earlier reminder from the local conversation.
// They are transient by design: keeping the old ones would accumulate one
// reminder per tool round and waste the window on stale lists.
func stripReminders(conversation []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(conversation))
	for _, message := range conversation {
		if !isReminder(message) {
			out = append(out, message)
		}
	}
	return out
}

// insertReminder puts one fresh reminder in the conversation. When the newest
// message is the user's question, the reminder goes just before it so they stay
// in the same trim turn: the sliding window always keeps the newest turn, and
// the question must not be separated from the reminder that serves it.
func insertReminder(conversation []provider.Message, text string) []provider.Message {
	conversation = stripReminders(conversation)
	reminder := provider.Message{Role: "user", Content: text}
	if n := len(conversation); n > 0 && isPlainUserMessage(conversation[n-1]) {
		out := make([]provider.Message, 0, n+1)
		out = append(out, conversation[:n-1]...)
		out = append(out, reminder, conversation[n-1])
		return out
	}
	return append(conversation, reminder)
}

func isPlainUserMessage(message provider.Message) bool {
	return message.Role == "user" && len(message.Images) == 0 &&
		message.ToolCallID == "" && !isReminder(message)
}

// prepareMessages trims the conversation and, when needed, injects the active
// goals and plans as a <system-remind> user message. A reminder is injected
// when a goal or plan tool changed something, or when the previous reminder no
// longer survived the sliding window. It is not injected for a chat that has no
// active goals or plans and did not just change them.
func (g *Gateway) prepareMessages(conversation []provider.Message, window session.Window, sessionID string, state *reminderState) ([]provider.Message, []provider.Message, bool) {
	messages := session.Trim(conversation, window)
	need := state.dirty || (!state.checkedEmpty && !hasReminder(messages))
	if need {
		text, active, err := g.objectiveReminder(sessionID)
		switch {
		case err != nil:
			// A failed reminder must not fail the turn. The next round retries
			// because dirty stays set only for an update; a read failure is
			// just logged and checked again on the next request.
			log.Printf("gateway: objective reminder: %v", err)
		case text != "" && (active || state.dirty):
			conversation = insertReminder(conversation, text)
			messages = session.Trim(conversation, window)
			state.checkedEmpty = !active
			state.dirty = false
		case !active:
			state.checkedEmpty = true
			state.dirty = false
		}
	}
	return conversation, messages, len(messages) == len(conversation)
}

func (g *Gateway) objectiveReminder(sessionID string) (string, bool, error) {
	if g.objectives == nil || strings.TrimSpace(sessionID) == "" {
		return "", false, nil
	}
	items, err := g.objectives.Open(sessionID)
	if err != nil {
		return "", false, err
	}
	return formatReminder(items), len(items) > 0, nil
}

func formatReminder(items []goals.Item) string {
	var goalsList, plansList []string
	for _, item := range items {
		line := "- [" + item.ID + "] " + item.Content
		switch item.Kind {
		case goals.Plan:
			plansList = append(plansList, line)
		default:
			goalsList = append(goalsList, line)
		}
	}

	var b strings.Builder
	b.WriteString(reminderOpen)
	b.WriteString("\nActive goals and plans in this chat. Keep them in mind; call goal_* or plan_* when one changes. ")
	b.WriteString("Completed and dropped items are removed from this list.\n")
	b.WriteString("\nGoals:\n")
	if len(goalsList) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, line := range goalsList {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nPlans:\n")
	if len(plansList) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, line := range plansList {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteString(reminderClose)
	return b.String()
}

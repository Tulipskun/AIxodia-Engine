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
// store, so a long tool loop does not query D1 every round for nothing. text is
// the reminder currently in force for this turn.
type reminderState struct {
	dirty        bool
	checkedEmpty bool
	text         string
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

// appendReminder puts one fresh reminder at the very end of the request, after
// trimming room for it. The tail is the cache-friendly position: the next
// request keeps every earlier message as a prefix, so replacing the reminder
// only invalidates the last message instead of the user's question. The
// reminder is trimmed separately from the conversation so the newest turn
// always survives even when the window is tight.
func appendReminder(conversation []provider.Message, text string, window session.Window) ([]provider.Message, bool) {
	conversation = stripReminders(conversation)
	reminder := provider.Message{Role: "user", Content: text}
	if window.Max <= 0 {
		window.Max = session.DefaultMaxTokens
	}
	window.Max -= session.MessageTokens(reminder)
	if window.Max < 1 {
		window.Max = 1
	}
	trimmed := session.Trim(conversation, window)
	out := make([]provider.Message, 0, len(trimmed)+1)
	out = append(out, trimmed...)
	out = append(out, reminder)
	return out, len(trimmed) == len(conversation)
}

// prepareMessages trims the conversation and, when needed, injects the active
// goals and plans as a <system-remind> user message at the tail. A reminder is
// injected when a goal or plan tool changed something, or when this turn has
// not looked at the store yet. It is not injected for a chat that has no active
// goals or plans and did not just change them.
func (g *Gateway) prepareMessages(conversation []provider.Message, window session.Window, sessionID string, state *reminderState) ([]provider.Message, []provider.Message, bool) {
	if window.Max <= 0 {
		window.Max = session.DefaultMaxTokens
	}
	// Older reminders are transient too. Strip them before trimming so the
	// newest turn is the user's question, not a stale reminder from the last
	// round.
	base := stripReminders(conversation)
	if len(base) != len(conversation) {
		// A provider count from the previous round covered the reminder that
		// was just removed, so it no longer describes this conversation's
		// prefix. Size the trimmed window from the estimate instead.
		window = session.Window{Max: window.Max}
	}
	need := state.dirty || (!state.checkedEmpty && state.text == "")
	if need {
		text, active, err := g.objectiveReminder(sessionID)
		switch {
		case err != nil:
			// A failed reminder must not fail the turn. The next round retries
			// because text stays empty or dirty stays set.
			log.Printf("gateway: objective reminder: %v", err)
		case text != "" && (active || state.dirty):
			state.text = text
			state.checkedEmpty = !active
			state.dirty = false
		case !active:
			state.text = ""
			state.checkedEmpty = true
			state.dirty = false
		}
	}

	if state.text != "" {
		out, wholeConversation := appendReminder(base, state.text, window)
		return out, out, wholeConversation && len(base) == len(conversation)
	}

	messages := session.Trim(base, window)
	return messages, messages, len(messages) == len(base) && len(base) == len(conversation)
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

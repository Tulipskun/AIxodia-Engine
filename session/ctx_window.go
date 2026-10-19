package session

import (
	"aixodia-engine/provider"
)

const (
	ChunkSize        = 4
	DefaultMaxTokens = 100000
)

func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return len(text)/ChunkSize + 1
}

func MessageTokens(message provider.Message) int {
	total := EstimateTokens(message.Content) + 4
	for _, call := range message.ToolCalls {
		total += EstimateTokens(call.Name) + EstimateTokens(call.Arguments) + 8
	}
	return total
}

func Tokens(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		total += MessageTokens(message)
	}
	return total
}

// Trim keeps the leading system messages plus the newest turns that fit inside
// maxTokens. It is a sliding window: when a conversation outgrows its budget the
// oldest history is dropped, never the question being asked.
//
// A turn starts at a user message and runs until the next one, so an assistant
// message and the tool results answering its calls always move together. The
// upstream refuses a request whose first non-system message is an assistant or a
// tool, and refuses a tool result whose call was cut away, so a boundary is only
// ever placed on a turn edge — never inside one.
func Trim(messages []provider.Message, maxTokens int) []provider.Message {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	system := 0
	for system < len(messages) && messages[system].Role == "system" {
		system++
	}
	body := messages[system:]

	budget := maxTokens - Tokens(messages[:system])
	if budget <= 0 {
		return messages
	}

	turns := splitTurns(body)

	total := 0
	for _, turn := range turns {
		total += Tokens(turn)
	}
	if total <= budget {
		return messages
	}

	// Walk the window in from the newest turn. The newest turn is kept even if
	// it alone is over budget: a request with nothing left to answer is worse
	// than one that is a little too long.
	start := len(turns)
	used := 0
	for start > 0 {
		tokens := Tokens(turns[start-1])
		if start < len(turns) && used+tokens > budget {
			break
		}
		used += tokens
		start--
	}

	// The body does not always open on a user message. Such a turn cannot lead
	// the request, so drop it: every later turn already starts at a user.
	if start < len(turns) && turns[start][0].Role != "user" {
		start++
	}

	trimmed := make([]provider.Message, 0, system)
	trimmed = append(trimmed, messages[:system]...)
	for _, turn := range turns[start:] {
		trimmed = append(trimmed, turn...)
	}
	return trimmed
}

// splitTurns cuts the body at every user message. Everything from a user message
// up to (but not including) the next one — the assistant's replies, the calls it
// made, and the results those calls returned — is one turn.
func splitTurns(messages []provider.Message) [][]provider.Message {
	var turns [][]provider.Message
	for _, message := range messages {
		if len(turns) == 0 || message.Role == "user" {
			turns = append(turns, nil)
		}
		last := len(turns) - 1
		turns[last] = append(turns[last], message)
	}
	return turns
}

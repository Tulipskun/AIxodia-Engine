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

// Trim keeps the leading system messages plus as many recent messages as fit
// inside maxTokens. Whole exchanges stay together: a message with tool calls
// is never kept while its tool results are dropped, and vice versa.
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

	groups := groupMessages(body)
	total := 0
	for _, group := range groups {
		total += Tokens(group)
	}
	if total <= budget {
		return messages
	}

	keep := len(groups)
	for keep > 1 && total > budget {
		keep--
		total -= Tokens(groups[keep-1])
	}

	trimmed := make([]provider.Message, 0, system+total)
	trimmed = append(trimmed, messages[:system]...)
	for _, group := range groups[:keep] {
		trimmed = append(trimmed, group...)
	}
	return trimmed
}

func groupMessages(messages []provider.Message) [][]provider.Message {
	var groups [][]provider.Message
	for _, message := range messages {
		if len(groups) == 0 || message.Role != "tool" {
			groups = append(groups, nil)
		}
		last := len(groups) - 1
		groups[last] = append(groups[last], message)
	}
	return groups
}

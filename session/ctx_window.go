package session

import (
	"encoding/json"
	"strconv"
	"unicode"

	"aixodia-engine/provider"
)

// DefaultMaxTokens is the budget a request is compacted to. It sits below the
// context window of the models the engine talks to, so the estimate that sizes
// the newest messages has room to be wrong without the request being refused.
const DefaultMaxTokens = 156000

// imageTokens is what one attached picture is counted as. A provider prices an
// image by its pixels, which the message does not carry, and the usual ceiling
// is about eleven hundred tokens; counting that much keeps a picture-heavy
// request from slipping past the window.
const imageTokens = 1100

// Character classes the estimator prices. A byte-pair tokenizer learns merges
// per script and per kind of character, so the runs below are what decide the
// cost, not the raw length of the string.
const (
	classNone  = iota
	classWord  // ASCII letters and digits
	classWide  // letters and digits outside ASCII
	classMark  // combining marks: part of the character before them
	classSpace // spaces, tabs, carriage returns
	classBreak // newlines
	classPunct // punctuation, symbols, emoji
)

func classOf(r rune) int {
	switch {
	case r == '\n':
		return classBreak
	case r == ' ' || r == '\t' || r == '\r' || r == '\v' || r == '\f':
		return classSpace
	case r < 0x80:
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return classWord
		}
		return classPunct
	case unicode.IsMark(r):
		return classMark
	case unicode.IsLetter(r) || unicode.IsNumber(r):
		return classWide
	default:
		return classPunct
	}
}

func runTokens(class, count int) int {
	switch class {
	case classWord:
		// English merges about four characters to a token, but a short word is
		// still one token, never a fraction of one.
		if n := (count + 2) / 4; n > 1 {
			return n
		}
		return 1
	case classWide:
		// A script the vocabularies learned less of costs close to a token per
		// character. Combining marks were skipped, so this counts syllables,
		// not the code points that spell them.
		return count
	case classPunct:
		// Punctuation and symbols mostly stand alone; runs like "==" or "..."
		// are single tokens often enough to price two to a token.
		if n := (count + 1) / 2; n > 0 {
			return n
		}
		return 1
	case classSpace:
		// One space rides on the word after it. Only a long indent costs.
		return count / 4
	case classBreak:
		return count
	}
	return count
}

// EstimateTokens counts text the way a byte-pair tokenizer does, as closely as
// this package can without the model's vocabulary. The old count was
// len(text)/4, which is only right for plain lower-case English: it charged
// digits, punctuation, whitespace and every non-Latin script the same four
// characters a token. Thai and CJK cost close to a token per character, a
// brace or bracket is usually its own token, and a newline is one too.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	total := 0
	class, count := classNone, 0
	for _, r := range text {
		k := classOf(r)
		if k == classMark {
			// A combining mark rides on the character before it and is not a
			// token of its own: Thai vowels and tone marks are separate code
			// points but one character to a tokenizer.
			continue
		}
		if k != class {
			if class != classNone {
				total += runTokens(class, count)
			}
			class, count = k, 0
		}
		count++
	}
	total += runTokens(class, count)
	return total
}

func MessageTokens(message provider.Message) int {
	total := EstimateTokens(message.Content) + 4
	// A reasoning model's earlier thoughts are replayed with its message, so
	// they occupy the prompt again on the next round.
	total += EstimateTokens(message.ReasoningContent)
	for _, call := range message.ToolCalls {
		total += EstimateTokens(call.Name) + EstimateTokens(call.Arguments) + 8
	}
	for range message.Images {
		total += imageTokens
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

// Window is the room a conversation has in the model's context. When a provider
// has reported the size of a prompt, that count is exact and covers the first
// Messages messages of the conversation it received; only what was appended
// after them has to be estimated. The zero Window estimates everything.
type Window struct {
	Max      int
	Usage    int // tokens the provider reported for the first Messages messages
	Messages int // how many leading messages that count covers
}

// Size is the conversation's size in tokens, anchored on the provider's count
// while that count still describes a prefix of the conversation.
func (w Window) Size(messages []provider.Message) int {
	if w.Messages > 0 && w.Messages <= len(messages) {
		return w.Usage + Tokens(messages[w.Messages:])
	}
	return Tokens(messages)
}

// Compact compacts a conversation that has reached the window. It replaces the
// sliding trim: instead of dropping the oldest turns until the budget fits, it
// keeps the newer turns that fit and hands the kept rounds back as the reason the
// model gave for them, not as the result text the call produced.
//
// The provider's own count decides whether the conversation fits, because it
// measured the prompt exactly; the estimate only has to size what was added
// after that measurement. Once a compact is needed the boundary is chosen from the
// estimate, since a suffix no longer matches the measured prefix.
//
// A turn starts at a user message and runs until the next one, so an assistant
// message and the tool results answering its calls always move together. The
// upstream refuses a request whose first non-system message is an assistant or a
// tool, and refuses a tool result whose call was cut away, so a boundary is only
// ever placed on a turn edge — never inside one.
func Compact(messages []provider.Message, window Window) []provider.Message {
	if window.Max <= 0 {
		window.Max = DefaultMaxTokens
	}

	if window.Size(messages) <= window.Max {
		return messages
	}

	system := 0
	for system < len(messages) && messages[system].Role == "system" {
		system++
	}
	body := messages[system:]

	budget := window.Max - Tokens(messages[:system])
	if budget <= 0 {
		return messages
	}

	turns := splitTurns(body)

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

	compacted := make([]provider.Message, 0, system)
	compacted = append(compacted, messages[:system]...)
	for _, turn := range turns[start:] {
		for _, message := range turn {
			compacted = append(compacted, message)
		}
	}

	// A kept round answers its call with the reason the model wrote for it, not
	// with the full output the call produced; the result text is what makes the
	// window grow, and the reason is all the next question ever needs.
	calls := toolCalls(compacted)
	for i := range compacted {
		if compacted[i].Role != "tool" {
			continue
		}
		if call, ok := calls[compacted[i].ToolCallID]; ok {
			if reason := toolReason(call); reason != "" {
				compacted[i].Content = reason
			}
		}
	}
	return compacted
}

// toolCalls indexes a conversation's tool calls by the call id a tool result
// answers, so a compacted round can hand the result back as the call's reason.
func toolCalls(messages []provider.Message) map[string]provider.ToolCall {
	calls := map[string]provider.ToolCall{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			calls[call.ID] = call
		}
	}
	return calls
}

// toolReason reads the short reason a model wrote for a call out of the
// arguments the call carried, which is where the tool's own parameters and the
// shared "reason" both arrive as JSON.
func toolReason(call provider.ToolCall) string {
	var fields map[string]any
	if json.Unmarshal([]byte(call.Arguments), &fields) != nil {
		return ""
	}
	switch value := fields["reason"].(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return ""
}

// Anchor reports what the provider last measured in a stored history. A turn
// that ran tools stores the sum of its rounds, which is not the size of any one
// prompt, so only a turn that answered in a single round can anchor; the newest
// such turn wins. messages is how many reconstructed messages that prompt
// carried, and tokens is the provider's count for them — which also includes the
// tool definitions, an over-count that errs on the safe side.
func Anchor(turns []Turn) (messages, tokens int) {
	seen := 0
	tools := false
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			tools = false
			seen++
		case "system":
			seen++
		case "assistant":
			// InputTokens is the prompt that produced this answer, so it covers
			// the messages before it, not the answer itself.
			if !tools && turn.InputTokens > 0 {
				messages, tokens = seen, turn.InputTokens
			}
			seen++
		case "tool_call":
			var row toolCallRow
			if json.Unmarshal([]byte(turn.Text), &row) != nil || row.ID == "" {
				continue
			}
			tools = true
			seen++
		case "tool_result":
			var row toolResultRow
			if json.Unmarshal([]byte(turn.Text), &row) != nil || row.ToolCallID == "" {
				continue
			}
			tools = true
			seen++
		}
	}
	return messages, tokens
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

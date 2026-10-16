package session

import (
	"encoding/json"
	"strings"

	"ai-engine/provider"
)

// Tool steps are stored as ordinary turns with role tool_call / tool_result so
// the phone can show them and the next request can replay them. Each row holds
// one call (or one result) as JSON in text. The assistant text that came before
// a group of calls rides on the first tool_call row of that group.

type toolCallRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   string `json:"content,omitempty"`
	// Reasoning rides on the call row rather than on the narration row: what
	// the next request has to send back is the assistant turn that made the
	// call.
	Reasoning string `json:"reasoning,omitempty"`
}

type toolResultRow struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
}

// TrailTurns converts the tool steps of one reply into storable turns.
func TrailTurns(trail []provider.Message) []TurnInput {
	var out []TurnInput
	narration := 0
	for _, msg := range trail {
		switch {
		case msg.Role == "assistant" && len(msg.ToolCalls) > 0:
			// The model's words before a tool call are its own turn, not part
			// of the call. Hiding them inside the call row put sentences the
			// user wrote to the model inside a tool block.
			if text := strings.TrimSpace(msg.Content); text != "" {
				narration++
				// Stored blocks render in order, so the id only has to be
				// unique inside the turn, not linked to a live request.
				out = append(out, TurnInput{
					Role: "assistant", Text: text,
					Block: TextBlock("n"+itoa(narration), text).JSON(),
				})
			}
			for _, call := range msg.ToolCalls {
				out = append(out, TurnInput{Role: "tool_call", Text: mustJSON(toolCallRow{
					ID:        call.ID,
					Name:      call.Name,
					Arguments: call.Arguments,
					Reasoning: msg.ReasoningContent,
				}), Block: ToolBlock(call.ID, call.Name, call.Arguments, "", false, 0).JSON()})
			}
		case msg.Role == "tool":
			content := msg.Content
			out = append(out, TurnInput{Role: "tool_result", Text: mustJSON(toolResultRow{
				ToolCallID: msg.ToolCallID,
				Name:       msg.ToolName,
				Content:    content,
			}), Block: ToolBlock(msg.ToolCallID, msg.ToolName, "", content, isErrorOutput(content), 0).JSON()})
		}
	}
	return out
}

// isErrorOutput mirrors gateway.isToolError: a failed execution is a
// {"error":...} envelope, not genuine command output. Kept here so the stored
// tool block carries the settled state without a second pass.
func isErrorOutput(output string) bool {
	trimmed := strings.TrimSpace(output)
	return strings.HasPrefix(trimmed, `{"error":`) || strings.HasPrefix(trimmed, `{"error" :`)
}

// itoa is fmt.Sprint without pulling fmt into every small file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// MessagesFromTurns rebuilds the conversation a model needs from stored turns.
// Rows that cannot be decoded are skipped rather than failing the whole chat.
func MessagesFromTurns(turns []Turn) []provider.Message {
	messages := make([]provider.Message, 0, len(turns))
	for _, turn := range turns {
		switch turn.Role {
		case "system", "user", "assistant":
			messages = append(messages, provider.Message{
				Role:             turn.Role,
				Content:          turn.Text,
				ReasoningContent: turn.ReasoningContent,
			})
		case "tool_call":
			var row toolCallRow
			if json.Unmarshal([]byte(turn.Text), &row) != nil || row.ID == "" {
				continue
			}
			messages = append(messages, provider.Message{
				Role:    "assistant",
				Content: row.Content,
				ToolCalls: []provider.ToolCall{{
					ID:        row.ID,
					Name:      row.Name,
					Arguments: row.Arguments,
				}},
				ReasoningContent: row.Reasoning,
			})
		case "tool_result":
			var row toolResultRow
			if json.Unmarshal([]byte(turn.Text), &row) != nil || row.ToolCallID == "" {
				continue
			}
			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: row.ToolCallID,
				ToolName:   row.Name,
				Content:    row.Content,
			})
		}
	}
	return messages
}

// IsToolMessage reports whether a message exists only because of tool use.
func IsToolMessage(msg provider.Message) bool {
	return msg.Role == "tool" || len(msg.ToolCalls) > 0
}

func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

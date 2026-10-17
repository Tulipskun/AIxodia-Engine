package session

import "encoding/json"
import "fmt"

// Display block types. The engine owns this vocabulary: it describes every
// visible unit of a turn — words, thoughts, tool use, one provider request,
// a transient status line — and the phone renders each type with one generic
// renderer. Changing how a turn looks means changing the blocks the engine
// emits, not shipping a new app: a type the app has never seen still draws
// as a plain card with its title and body, so new kinds of content never
// need an app update to be readable.
const (
	// BlockText is narration or an answer: the model's own words.
	BlockText = "text"
	// BlockReasoning is what the model thought before it acted.
	BlockReasoning = "reasoning"
	// BlockTool is one tool call and, when it lands, its result.
	BlockTool = "tool"
	// BlockRequest is one provider request round of a turn: waiting for its
	// first byte, thinking once reasoning arrives, done when the round ends.
	BlockRequest = "request"
	// BlockStatus is a transient line ("กำลังส่งให้ provider…") that names
	// what the turn is doing right now. It never lands in storage.
	BlockStatus = "status"
)

// Block states, per type. A request walks waiting → thinking → done; a tool
// walks running → done (or error); text and reasoning are done when stored.
const (
	StateWaiting  = "waiting"
	StateThinking = "thinking"
	StateRunning  = "running"
	StateDone     = "done"
	StateError    = "error"
)

// Block is one visible unit of a turn, as the engine describes it. Flat on
// purpose: every field fits every type, and a renderer reads only the fields
// its type needs, so adding a field never breaks an older phone — unknown
// fields are ignored the way unknown types fall back to a plain card.
type Block struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	State      string `json:"state,omitempty"`
	Text       string `json:"text,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	ThinkingMs int64  `json:"thinking_ms,omitempty"`
	Round      int    `json:"round,omitempty"`
}

// JSON renders the block the way it rides on a turn and on a live frame.
func (b Block) JSON() string {
	encoded, err := json.Marshal(b)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ParseBlock decodes a stored block, reporting false when it is missing or
// not a block at all. Old rows predate blocks and take the role-based path
// on the phone instead.
func ParseBlock(s string) (Block, bool) {
	var b Block
	if s == "" {
		return b, false
	}
	if err := json.Unmarshal([]byte(s), &b); err != nil || b.Type == "" {
		return Block{}, false
	}
	return b, true
}

// RequestID names the block of one provider request round: the request's
// own lifecycle and the reasoning that belongs to it share it, so the phone
// can grow the right block no matter which event arrives first. The format is
// part of the protocol ("req<N}"): a tool event carries only its round, and
// the phone derives this id to settle the round that spoke.
func RequestID(round int) string {
	return fmt.Sprintf("req%d", round)
}

// RequestBlock opens (or settles) one provider request round.
func RequestBlock(round int, state string) Block {
	return Block{ID: RequestID(round), Type: BlockRequest, State: state, Round: round}
}

// ReasoningBlock is one chunk of what the round is thinking. Chunks share
// the request's id because they belong to that request, not to the turn.
func ReasoningBlock(round int, text string) Block {
	return Block{ID: RequestID(round), Type: BlockReasoning, Text: text, Round: round}
}

// TextBlock is the model's own words: narration before a tool call, or the
// closing answer. Stored rows carry it; live words still stream as deltas.
func TextBlock(id, text string) Block {
	return Block{ID: id, Type: BlockText, State: StateDone, Text: text}
}

// ToolBlock is one tool step. A call without a result is still running; a
// result settles it, error or not.
func ToolBlock(callID, name, args, result string, isError bool, durationMs int64) Block {
	state := StateRunning
	if result != "" || isError {
		state = StateDone
		if isError {
			state = StateError
		}
	}
	return Block{
		ID: callID, Type: BlockTool, State: state,
		Name: name, Arguments: args, Result: result,
		IsError: isError, DurationMs: durationMs,
	}
}

// StatusBlock is a transient line. It is never stored: re-reading history
// shows what happened, not what was happening.
func StatusBlock(text string) Block {
	return Block{ID: "status", Type: BlockStatus, Text: text}
}

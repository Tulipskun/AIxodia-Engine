// Package mobile serves the WebSocket the AIxodia app talks to: one socket per
// phone, frames are JSON, auth is the bearer token in the handshake only.
// The RFC 6455 handshake and framing come from io/wsock, shared with the
// terminal socket.
package mobile

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"aixodia-engine/io/wsock"
	"aixodia-engine/session"
)

// ChatFunc answers one user message in a session, calling onDelta for each
// streamed piece and onTool for each live tool activity, and returns the
// committed turn data. It must stop when ctx is cancelled.
// Attachments are the files the phone uploaded
// before sending it: the pictures among them go to the model as pictures, and
// every one of them is still named in the text for the model that cannot take
// them or for a later turn that reads this back from the database.
type ChatFunc func(ctx context.Context, sessionID, text string, attachments []Attachment, onDelta func(string), onTool func(ToolEvent)) (ChatResult, error)

// ChatResult carries committed rows, not a socket-local sequence counter.
type ChatResult struct {
	Answer      session.Turn
	Turns       []session.Turn
	TurnsSynced bool
}

// Attachment is one uploaded file as the phone describes it.
type Attachment struct {
	Name string
	Path string
	Mime string
}

// ToolEvent is one live tool activity inside a turn.
type ToolEvent struct {
	Stage      string
	Name       string
	CallID     string
	Arguments  string
	Text       string
	IsError    bool
	DurationMs int64
	// Round is the provider request round the event belongs to, and State is
	// where that round or step stands (waiting/thinking/running/done/error).
	Round int
	State string
	// Usage rides the stage="usage" event: the turn's running token total after
	// a provider round, so the phone can show the cost climbing while the turn
	// still runs. They are zero on every other stage.
	PromptTokens       int
	CompletionTokens   int
	CacheReadTokens    int
	CacheWriteTokens   int
	ReasoningTokens    int
	InputIncludesCache bool
}

type inFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	ClientMsgID string `json:"client_msg_id"`
	JobID       string `json:"job_id"`
	// Attachments are files the phone already uploaded. Their daemon-side paths
	// are handed to the agent, which reads them with its own tools.
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

type outFrame struct {
	Source       string          `json:"source"`
	SessionID    string          `json:"session_id"`
	Kind         string          `json:"kind"`
	Text         string          `json:"text,omitempty"`
	Role         string          `json:"role,omitempty"`
	Agent        string          `json:"agent,omitempty"`
	ClientMsgID  string          `json:"client_msg_id,omitempty"`
	Stage        string          `json:"stage,omitempty"`
	ToolCall     *toolCallJSON   `json:"tool_call,omitempty"`
	ToolResult   *toolResultJSON `json:"tool_result,omitempty"`
	ToolDuration int64           `json:"tool_duration_ms,omitempty"`
	// Block is the display block for this frame, described by the engine so
	// the phone renders it without knowing the stage vocabulary. Frames from
	// an older daemon carry nothing here and take the stage-based path.
	Block              *session.Block `json:"block,omitempty"`
	Seq                int64          `json:"seq,omitempty"`
	InputTokens        int            `json:"input_tokens,omitempty"`
	OutputTokens       int            `json:"output_tokens,omitempty"`
	CacheReadTokens    int            `json:"cache_read_tokens"`
	CacheWriteTokens   int            `json:"cache_write_tokens"`
	ReasoningTokens    int            `json:"reasoning_tokens"`
	ReasoningContent   string         `json:"reasoning_content,omitempty"`
	InputIncludesCache bool           `json:"input_includes_cache"`
	Model              string         `json:"model,omitempty"`
	DurationMS         int64          `json:"duration_ms,omitempty"`
	GenerationMS       int64          `json:"generation_ms,omitempty"`
	Turns              []session.Turn `json:"turns,omitempty"`
	TurnsSynced        bool           `json:"turns_synced"`
}

type toolCallJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolResultJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// Hub tracks every connected phone so the daemon can push a frame to all of
// them at once, with no polling and no second connection per phone.
type Hub struct {
	mu    sync.Mutex
	peers map[*peer]struct{}
}

// NewHub builds a hub.
func NewHub() *Hub {
	return &Hub{peers: map[*peer]struct{}{}}
}

func (h *Hub) add(p *peer) {
	h.mu.Lock()
	h.peers[p] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(p *peer) {
	h.mu.Lock()
	delete(h.peers, p)
	h.mu.Unlock()
}

// Handler upgrades /ws to a WebSocket after checking the bearer token. It is a
// method on Hub so the connection is registered for broadcasts.
func (h *Hub) Handler(token string, chat ChatFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wsock.BearerOK(r, token) {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		conn, br, err := wsock.Accept(w, r)
		switch {
		case errors.Is(err, wsock.ErrNotUpgrade):
			http.Error(w, "websocket upgrade required", http.StatusBadRequest)
			return
		case errors.Is(err, wsock.ErrNoHijack):
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		case err != nil:
			return
		}
		defer conn.Close()
		h.serve(conn, br, chat)
	})
}

// Handler serves /ws. Kept for tests and simple callers.
func Handler(token string, chat ChatFunc) http.Handler {
	return NewHub().Handler(token, chat)
}

// activeTurn is one turn this peer is running, kept so a cancel frame can name
// the chat to stop instead of stopping every chat on the socket.
type activeTurn struct {
	sessionID string
	cancel    context.CancelFunc
}

type peer struct {
	conn    net.Conn
	mu      sync.Mutex
	cancels map[string]activeTurn
}

func (p *peer) send(f outFrame) error {
	f.Source = "ai"
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return wsock.WriteFrame(p.conn, wsock.OpText, data)
}

// ServeConn serves an already-established connection: the relay hands it the
// virtual connection that carries the phone's frames, so the socket policy
// (bearer, upgrade) has happened elsewhere and the hub only speaks frames.
func (h *Hub) ServeConn(conn net.Conn, chat ChatFunc) {
	h.serve(conn, bufio.NewReader(conn), chat)
}

func (h *Hub) serve(conn net.Conn, r *bufio.Reader, chat ChatFunc) {
	p := &peer{conn: conn, cancels: map[string]activeTurn{}}
	h.add(p)
	defer h.remove(p)
	// A socket that simply stops talking is a network event, not a decision by
	// the phone: the tunnel drops, the screen locks, the phone changes network,
	// and the turn running on it is cancelled with it — so the work stops and
	// the question is left unanswered. The turn carries its own context and its
	// answer is persisted either way, so it keeps going; on reconnect the phone
	// pulls the newest turns and finds the answer there. Only a close frame is
	// the phone saying it is finished, and that one cancels.
	for {
		op, payload, err := wsock.ReadFrame(r)
		if err != nil {
			return
		}
		switch op {
		case wsock.OpText:
			var in inFrame
			if err := json.Unmarshal(payload, &in); err != nil {
				_ = p.send(outFrame{Kind: "error", Text: "frame ที่อ่านไม่ได้"})
				continue
			}
			handle(p, in, chat)
		case wsock.OpClose:
			p.mu.Lock()
			_ = wsock.WriteFrame(conn, wsock.OpClose, []byte{})
			p.mu.Unlock()
			p.cancelSession("")
			return
		case wsock.OpPing:
			p.mu.Lock()
			_ = wsock.WriteFrame(conn, wsock.OpPong, payload)
			p.mu.Unlock()
		}
	}
}

func (p *peer) register(id, sessionID string, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels[id] = activeTurn{sessionID: sessionID, cancel: cancel}
}

func (p *peer) unregister(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cancels, id)
}

// cancelSession stops the turns running for sessionID and returns how many it
// stopped. An empty sessionID stops every turn on the peer, which is what a
// dropped socket does. A turn whose own session is unknown is stopped too: it
// is better to stop a turn the phone asked to stop than to leave it running.
func (p *peer) cancelSession(sessionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	stopped := 0
	for id, turn := range p.cancels {
		if sessionID != "" && turn.sessionID != "" && turn.sessionID != sessionID {
			continue
		}
		turn.cancel()
		delete(p.cancels, id)
		stopped++
	}
	return stopped
}

// handle answers hello and cancel inline; a message runs in its own goroutine
// so a cancel frame can arrive while the reply is still streaming.
func handle(p *peer, in inFrame, chat ChatFunc) {
	switch in.Type {
	case "hello":
		_ = p.send(outFrame{Kind: "ack", SessionID: in.SessionID})
	case "cancel":
		// Nothing may be running for this chat — the turn already ended, or the
		// phone is holding a screen the daemon finished with. The running turn
		// answers for itself (see answer), but when there is none, answer here
		// anyway: a stop that gets no reply leaves the screen stuck showing a
		// turn that is not happening.
		if p.cancelSession(in.SessionID) == 0 {
			_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, Stage: "already_done"})
		}
	case "message", "":
		var parts []string
		for _, c := range in.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text := strings.Join(parts, "\n")
		_ = p.send(outFrame{Kind: "ack", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID})
		if note := attachmentNote(in.Attachments); note != "" {
			// The note travels with the message, so the stored turn shows what
			// was attached and the agent is told where to find it.
			if strings.TrimSpace(text) == "" {
				text = note
			} else {
				text = text + "\n\n" + note
			}
		}
		if strings.TrimSpace(text) == "" {
			_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: "ข้อความว่าง"})
			return
		}
		// Register the cancel func before answering, so a cancel frame that
		// arrives right behind this message always finds it.
		ctx, cancel := context.WithCancel(context.Background())
		key := in.ClientMsgID
		if key == "" {
			key = in.SessionID
		}
		p.register(key, in.SessionID, cancel)
		go p.answer(ctx, cancel, key, in, text, chat)
	}
}

// attachmentsOf carries the uploaded files from the socket frame into the
// answer call, where the ones that are pictures can be sent to the model.
func attachmentsOf(items []attachment) []Attachment {
	if len(items) == 0 {
		return nil
	}
	out := make([]Attachment, 0, len(items))
	for _, a := range items {
		out = append(out, Attachment{Name: a.Name, Path: a.Path, Mime: a.Mime})
	}
	return out
}

// attachmentNote is the line appended to a message that carried files, so the
// agent knows what was attached and where each file sits on disk.
func attachmentNote(items []attachment) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[ไฟล์แนบ]")
	for _, a := range items {
		b.WriteString("\n- ")
		if a.Name != "" {
			b.WriteString(a.Name)
		} else {
			b.WriteString("ไฟล์")
		}
		if a.Path != "" {
			b.WriteString(" → ")
			b.WriteString(a.Path)
		}
		if a.Mime != "" {
			b.WriteString(" (")
			b.WriteString(a.Mime)
			b.WriteString(")")
		}
	}
	b.WriteString("\nเปิดอ่านได้ด้วย tool read/bash ตามพาธด้านบน")
	return b.String()
}

func (p *peer) answer(ctx context.Context, cancel context.CancelFunc, key string, in inFrame, text string, chat ChatFunc) {
	defer cancel()
	defer p.unregister(key)
	// This runs in its own goroutine, so a panic here would take the whole
	// daemon down. Report it to the phone as an error instead.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mobile: answer panicked: %v", r)
			_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: "เกิดข้อผิดพลาดภายในระบบ ลองส่งอีกครั้ง"})
		}
	}()

	onDelta := func(piece string) {
		_ = p.send(outFrame{Kind: "delta", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: piece, Role: "model", Agent: "main"})
	}
	onTool := func(ev ToolEvent) {
		frame := outFrame{Kind: "trace", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Agent: "main", Stage: ev.Stage, ToolDuration: ev.DurationMs, Block: toolBlock(ev)}
		switch ev.Stage {
		case "tool_call":
			frame.ToolCall = &toolCallJSON{ID: ev.CallID, Name: ev.Name, Arguments: ev.Arguments}
		case "tool_result":
			frame.ToolResult = &toolResultJSON{ID: ev.CallID, Name: ev.Name, Text: ev.Text, IsError: ev.IsError}
			frame.ToolCall = &toolCallJSON{ID: ev.CallID, Name: ev.Name}
		case "usage":
			// The turn's running cost, re-stated after each provider round.
			// No text and no block: this frame moves the strip, not the thread.
			frame.InputTokens = ev.PromptTokens
			frame.OutputTokens = ev.CompletionTokens
			frame.CacheReadTokens = ev.CacheReadTokens
			frame.CacheWriteTokens = ev.CacheWriteTokens
			frame.ReasoningTokens = ev.ReasoningTokens
			frame.InputIncludesCache = ev.InputIncludesCache
		default:
			frame.Text = ev.Text
		}
		_ = p.send(frame)
	}
	result, err := chat(ctx, in.SessionID, text, attachmentsOf(in.Attachments), onDelta, onTool)
	if ctx.Err() != nil {
		_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Stage: "cancelled", Turns: result.Turns, TurnsSynced: result.TurnsSynced})
		return
	}
	if err != nil {
		log.Printf("mobile: chat: %v", err)
		_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: err.Error(), Turns: result.Turns, TurnsSynced: result.TurnsSynced})
		return
	}
	answer := result.Answer
	answerBlock := (*session.Block)(nil)
	if b, ok := session.ParseBlock(answer.Block); ok {
		answerBlock = &b
	}
	_ = p.send(outFrame{Kind: "message", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID,
		Text: answer.Text, Role: "model", Agent: "main", Seq: int64(answer.Seq),
		Block:       answerBlock,
		InputTokens: answer.InputTokens, OutputTokens: answer.OutputTokens,
		CacheReadTokens: answer.CacheReadTokens, CacheWriteTokens: answer.CacheWriteTokens,
		ReasoningTokens: answer.ReasoningTokens, ReasoningContent: answer.ReasoningContent,
		InputIncludesCache: answer.InputIncludesCache, Model: answer.Model,
		DurationMS: answer.DurationMS, GenerationMS: answer.GenerationMS})
	_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Seq: int64(answer.Seq), Turns: result.Turns, TurnsSynced: result.TurnsSynced})
}

// toolBlock describes a live tool event the way the stored row will: one
// block per event, keyed the same, so the live thread and the stored thread
// it becomes are the same blocks. Unknown stages carry no block and take the
// text path, the way an older daemon's frames do.
func toolBlock(ev ToolEvent) *session.Block {
	var b session.Block
	switch ev.Stage {
	case "tool_call":
		b = session.ToolBlock(ev.CallID, ev.Name, ev.Arguments, "", false, 0)
	case "tool_result":
		b = session.ToolBlock(ev.CallID, ev.Name, ev.Arguments, ev.Text, ev.IsError, ev.DurationMs)
	case "request":
		b = session.RequestBlock(ev.Round, ev.State)
	case "reasoning":
		b = session.ReasoningBlock(ev.Round, ev.Text)
	default:
		return nil
	}
	return &b
}

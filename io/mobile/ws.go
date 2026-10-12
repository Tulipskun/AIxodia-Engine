// Package mobile serves the WebSocket the AIxodia app talks to: one socket per
// phone, frames are JSON, auth is the bearer token in the handshake only.
// Stdlib only (RFC 6455 framing is implemented here).
package mobile

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const maxFrame = 1 << 20

// ChatFunc answers one user message in a session, calling onDelta for each
// streamed piece and onTool for each live tool activity, and returns the
// full reply plus token counts. It must stop when ctx is cancelled.
// ChatFunc answers one message. Attachments are the files the phone uploaded
// before sending it: the pictures among them go to the model as pictures, and
// every one of them is still named in the text for the model that cannot take
// them or for a later turn that reads this back from the database.
type ChatFunc func(ctx context.Context, sessionID, text string, attachments []Attachment, onDelta func(string), onTool func(ToolEvent)) (reply string, inTokens, outTokens int, err error)

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
	Seq          int64           `json:"seq,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	// UI carries the server-driven UI spec on a kind="ui" frame. It is sent
	// when a phone connects and again whenever the spec changes, so the app
	// never has to poll for it.
	UI json.RawMessage `json:"ui,omitempty"`
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
// them at once. This is how a change on the server (a new UI spec) reaches the
// app with no polling and no second connection.
type Hub struct {
	mu    sync.Mutex
	peers map[*peer]struct{}
	// spec, when set, is sent to each phone as soon as it connects, so a client
	// is current without asking.
	spec func() json.RawMessage
}

// NewHub builds a hub. spec may be nil when there is nothing to push on connect.
func NewHub(spec func() json.RawMessage) *Hub {
	return &Hub{peers: map[*peer]struct{}{}, spec: spec}
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

// BroadcastUI sends the UI spec to every connected phone. A phone whose socket
// has died is dropped here rather than left to fail on its next write.
func (h *Hub) BroadcastUI(spec json.RawMessage) {
	if len(spec) == 0 {
		return
	}
	frame := outFrame{Kind: "ui", UI: spec}
	h.mu.Lock()
	peers := make([]*peer, 0, len(h.peers))
	for p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	for _, p := range peers {
		if err := p.send(frame); err != nil {
			h.remove(p)
		}
	}
}

// Handler upgrades /ws to a WebSocket after checking the bearer token. It is a
// method on Hub so the connection is registered for broadcasts.
func (h *Hub) Handler(token string, chat ChatFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
			http.Error(w, "websocket upgrade required", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err := rw.Flush(); err != nil {
			return
		}
		h.serve(conn, rw.Reader, chat)
	})
}

// Handler serves /ws with no push support. Kept for tests and simple callers.
func Handler(token string, chat ChatFunc) http.Handler {
	return NewHub(nil).Handler(token, chat)
}

type peer struct {
	conn    net.Conn
	mu      sync.Mutex
	seq     int64
	cancels map[string]context.CancelFunc
}

func (p *peer) send(f outFrame) error {
	f.Source = "ai"
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return writeFrame(p.conn, 0x1, data)
}

func (h *Hub) serve(conn net.Conn, r *bufio.Reader, chat ChatFunc) {
	p := &peer{conn: conn, cancels: map[string]context.CancelFunc{}}
	h.add(p)
	defer h.remove(p)
	defer p.cancelAll()
	// A phone that just connected gets the current spec immediately; after that
	// every change is pushed by BroadcastUI.
	if h.spec != nil {
		if spec := h.spec(); len(spec) > 0 {
			_ = p.send(outFrame{Kind: "ui", UI: spec})
		}
	}
	for {
		op, payload, err := readFrame(r)
		if err != nil {
			return
		}
		switch op {
		case 0x1: // text
			var in inFrame
			if err := json.Unmarshal(payload, &in); err != nil {
				_ = p.send(outFrame{Kind: "error", Text: "frame ที่อ่านไม่ได้"})
				continue
			}
			handle(p, in, chat)
		case 0x8: // close
			p.mu.Lock()
			_ = writeFrame(conn, 0x8, []byte{})
			p.mu.Unlock()
			return
		case 0x9: // ping
			p.mu.Lock()
			_ = writeFrame(conn, 0xA, payload)
			p.mu.Unlock()
		}
	}
}

func (p *peer) register(id string, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels[id] = cancel
}

func (p *peer) unregister(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cancels, id)
}

func (p *peer) cancelAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, cancel := range p.cancels {
		cancel()
		delete(p.cancels, id)
	}
}

// handle answers hello and cancel inline; a message runs in its own goroutine
// so a cancel frame can arrive while the reply is still streaming.
func handle(p *peer, in inFrame, chat ChatFunc) {
	switch in.Type {
	case "hello":
		_ = p.send(outFrame{Kind: "ack", SessionID: in.SessionID})
	case "cancel":
		p.cancelAll()
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
		p.register(key, cancel)
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
		frame := outFrame{Kind: "trace", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Agent: "main", Stage: ev.Stage, ToolDuration: ev.DurationMs}
		switch ev.Stage {
		case "tool_call":
			frame.ToolCall = &toolCallJSON{ID: ev.CallID, Name: ev.Name, Arguments: ev.Arguments}
		case "tool_result":
			frame.ToolResult = &toolResultJSON{ID: ev.CallID, Name: ev.Name, Text: ev.Text, IsError: ev.IsError}
			frame.ToolCall = &toolCallJSON{ID: ev.CallID, Name: ev.Name}
		default:
			frame.Text = ev.Text
		}
		_ = p.send(frame)
	}
	reply, inTok, outTok, err := chat(ctx, in.SessionID, text, attachmentsOf(in.Attachments), onDelta, onTool)
	if ctx.Err() != nil {
		_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: "cancelled"})
		return
	}
	if err != nil {
		log.Printf("mobile: chat: %v", err)
		_ = p.send(outFrame{Kind: "error", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Text: err.Error()})
		return
	}
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()
	_ = p.send(outFrame{Kind: "message", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID,
		Text: reply, Role: "model", Agent: "main", Seq: seq, InputTokens: inTok, OutputTokens: outTok})
	_ = p.send(outFrame{Kind: "done", SessionID: in.SessionID, ClientMsgID: in.ClientMsgID, Seq: seq})
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func writeFrame(w io.Writer, op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

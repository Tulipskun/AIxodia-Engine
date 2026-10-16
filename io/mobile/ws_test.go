package mobile

import (
	"ai-engine/session"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeChat(ctx context.Context, sessionID, text string, attachments []Attachment, onDelta func(string), onTool func(ToolEvent)) (ChatResult, error) {
	onDelta("echo: ")
	onDelta(text)
	return ChatResult{Answer: session.Turn{Text: "echo: " + text, InputTokens: 3, OutputTokens: 4}}, nil
}

func TestClosingFramesCarryActualSeqAndFullTurnBatch(t *testing.T) {
	answer := session.Turn{SessionID: "s1", Seq: 42, Role: "assistant", Text: "answer", Model: "big-pickle",
		InputTokens: 100, OutputTokens: 5, CacheReadTokens: 80, CacheWriteTokens: 2,
		ReasoningTokens: 3, ReasoningContent: "real reasoning", InputIncludesCache: true,
		DurationMS: 9000, GenerationMS: 4000, CreatedAt: 123}
	chat := func(ctx context.Context, sid, text string, a []Attachment, delta func(string), tool func(ToolEvent)) (ChatResult, error) {
		return ChatResult{Answer: answer, Turns: []session.Turn{{SessionID: "s1", Seq: 40, Role: "user", Text: text}, answer}, TurnsSynced: true}, nil
	}
	srv := httptest.NewServer(Handler("good", chat))
	defer srv.Close()
	conn, r, _ := dialWS(t, srv, "good")
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	sendMasked(t, conn, []byte(`{"type":"message","session_id":"s1","client_msg_id":"c1","content":[{"text":"hi"}]}`))
	for {
		_, payload, err := readFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		var f outFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			t.Fatal(err)
		}
		if f.Kind == "message" && (f.Seq != 42 || f.CacheReadTokens != 80 || f.CacheWriteTokens != 2 || f.ReasoningTokens != 3 || !f.InputIncludesCache || f.ReasoningContent != "real reasoning" || f.DurationMS != 9000 || f.GenerationMS != 4000 || f.Model != "big-pickle") {
			t.Fatalf("incomplete message: %+v", f)
		}
		if f.Kind == "done" {
			if f.Seq != 42 || !f.TurnsSynced || len(f.Turns) != 2 || f.Turns[1] != answer {
				t.Fatalf("bad done: %+v", f)
			}
			break
		}
	}
}

func dialWS(t *testing.T, srv *httptest.Server, token string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer %s\r\n\r\n", addr, k, token)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, r, resp.StatusCode
}

func sendMasked(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	mask := []byte{1, 2, 3, 4}
	hdr := []byte{0x81}
	n := len(payload)
	if n < 126 {
		hdr = append(hdr, 0x80|byte(n))
	} else {
		t.Fatal("test payload too large")
	}
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := conn.Write(append(hdr, masked...)); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(Handler("good", fakeChat))
	defer srv.Close()
	_, _, code := dialWS(t, srv, "bad")
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	srv := httptest.NewServer(Handler("good", fakeChat))
	defer srv.Close()
	conn, r, code := dialWS(t, srv, "good")
	defer conn.Close()
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("want 101, got %d", code)
	}
	sendMasked(t, conn, []byte(`{"type":"hello","session_id":"s1"}`))
	sendMasked(t, conn, []byte(`{"type":"message","session_id":"s1","client_msg_id":"c1","content":[{"type":"text","text":"hi"}]}`))

	var kinds []string
	var msg outFrame
	for len(kinds) < 6 {
		op, payload, err := readFrame(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if op != 0x1 {
			continue
		}
		var f outFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, f.Kind)
		if f.Kind == "message" {
			msg = f
		}
	}
	want := []string{"ack", "ack", "delta", "delta", "message", "done"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("frames %v, want %v", kinds, want)
	}
	if msg.Text != "echo: hi" || msg.ClientMsgID != "c1" || msg.Role != "model" || msg.Source != "ai" {
		t.Fatalf("bad message frame: %+v", msg)
	}
}

// sendClose writes a masked close frame, the way the phone says it is finished.
func sendClose(t *testing.T, conn net.Conn) {
	t.Helper()
	mask := []byte{9, 8, 7, 6}
	frame := append([]byte{0x88, 0x80}, mask...)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
}

// parkedChat is a turn that waits to be released, so a test can decide when it
// finishes and see whether it was cancelled instead.
func parkedChat(started chan<- struct{}, release <-chan struct{}, finished chan<- error) ChatFunc {
	return func(ctx context.Context, sessionID, text string, attachments []Attachment, onDelta func(string), onTool func(ToolEvent)) (ChatResult, error) {
		close(started)
		select {
		case <-release:
			finished <- nil
			return ChatResult{Answer: session.Turn{Text: "answered"}}, nil
		case <-ctx.Done():
			finished <- ctx.Err()
			return ChatResult{}, ctx.Err()
		}
	}
}

func startParkedTurn(t *testing.T, chat ChatFunc) (net.Conn, chan struct{}, chan struct{}, chan error) {
	t.Helper()
	srv := httptest.NewServer(Handler("good", chat))
	t.Cleanup(srv.Close)
	conn, _, code := dialWS(t, srv, "good")
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("want 101, got %d", code)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, started, release, finished
}

// A socket that dies on its own is the tunnel or the network, not the phone
// cancelling. The turn keeps running so the work is not lost mid-way; the answer
// is persisted and the phone picks it up when it reconnects.
func TestDroppedSocketDoesNotCancelTheTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	conn, _, _, _ := startParkedTurn(t, parkedChat(started, release, finished))

	sendMasked(t, conn, []byte(`{"type":"message","session_id":"s1","client_msg_id":"c1","content":[{"type":"text","text":"hi"}]}`))
	<-started
	conn.Close()
	time.Sleep(50 * time.Millisecond) // give serve time to notice the dead socket
	close(release)

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("turn was cancelled by the dropped socket: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never finished")
	}
}

// A stop with no turn running must still be answered: the phone is showing a
// turn the daemon has already finished with, and with no reply the screen stays
// stuck on it forever.
func TestCancelWithNothingRunningAnswersAlreadyDone(t *testing.T) {
	srv := httptest.NewServer(Handler("good", fakeChat))
	defer srv.Close()
	conn, r, _ := dialWS(t, srv, "good")
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	sendMasked(t, conn, []byte(`{"type":"cancel","session_id":"s1"}`))
	for {
		_, payload, err := readFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		var f outFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			t.Fatal(err)
		}
		if f.Kind != "done" {
			continue
		}
		if f.Stage != "already_done" || f.SessionID != "s1" {
			t.Fatalf("done = %+v, want already_done for s1", f)
		}
		return
	}
}

// A stop names one chat. A turn running for another chat on the same socket
// must keep running, or a stop in one thread would kill another's work.
func TestCancelForAnotherChatDoesNotStopTheRunningOne(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	conn, _, _, _ := startParkedTurn(t, parkedChat(started, release, finished))

	sendMasked(t, conn, []byte(`{"type":"message","session_id":"s1","client_msg_id":"c1","content":[{"type":"text","text":"hi"}]}`))
	<-started
	sendMasked(t, conn, []byte(`{"type":"cancel","session_id":"s2"}`))
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("a cancel for another chat stopped this turn: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never finished")
	}
}

// A close frame is the phone saying it is finished, and it stops the turn.
func TestCloseFrameCancelsTheTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	conn, _, _, _ := startParkedTurn(t, parkedChat(started, release, finished))

	sendMasked(t, conn, []byte(`{"type":"message","session_id":"s1","client_msg_id":"c1","content":[{"type":"text","text":"hi"}]}`))
	<-started
	sendClose(t, conn)

	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("a close frame must stop the turn")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn was not cancelled")
	}
}

func TestAttachmentNote(t *testing.T) {
	if got := attachmentNote(nil); got != "" {
		t.Errorf("empty attachments should add nothing, got %q", got)
	}
	got := attachmentNote([]attachment{{Name: "a.jpg", Path: "/tmp/a.jpg", Mime: "image/jpeg"}})
	if !strings.Contains(got, "a.jpg") || !strings.Contains(got, "/tmp/a.jpg") {
		t.Errorf("note missing name/path: %q", got)
	}
}

func TestAttachmentNoteListsEveryFile(t *testing.T) {
	got := attachmentNote([]attachment{
		{Name: "a.png", Path: "/u/s/a.png", Mime: "image/png"},
		{Name: "b.pdf", Path: "/u/s/b.pdf", Mime: "application/pdf"},
		{Name: "c.txt", Path: "/u/s/c.txt"},
	})
	for _, want := range []string{"a.png", "/u/s/a.png", "image/png", "b.pdf", "/u/s/b.pdf", "c.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("note is missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(got, "\n- "); lines != 3 {
		t.Errorf("note has %d file lines, want 3:\n%s", lines, got)
	}
}

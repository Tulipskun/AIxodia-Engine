package mobile

import (
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
)

func fakeChat(ctx context.Context, sessionID, text string, attachments []Attachment, onDelta func(string), onTool func(ToolEvent)) (string, int, int, error) {
	onDelta("echo: ")
	onDelta(text)
	return "echo: " + text, 3, 4, nil
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

func TestHubBroadcastUIFrame(t *testing.T) {
	hub := NewHub(nil)
	c1, c2 := net.Pipe()
	p := &peer{conn: c1, cancels: map[string]context.CancelFunc{}}
	hub.add(p)
	defer c2.Close()

	go func() { hub.BroadcastUI(json.RawMessage(`{"version":7}`)) }()

	op, payload, err := readFrame(bufio.NewReader(c2))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != 0x1 {
		t.Fatalf("op = %x, want text", op)
	}
	var f map[string]any
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatalf("frame json: %v", err)
	}
	if f["kind"] != "ui" {
		t.Errorf("kind = %v, want ui", f["kind"])
	}
	if _, ok := f["ui"].(map[string]any); !ok {
		t.Errorf("ui missing or not an object: %v", f["ui"])
	}
	if f["source"] != "ai" {
		t.Errorf("source = %v", f["source"])
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

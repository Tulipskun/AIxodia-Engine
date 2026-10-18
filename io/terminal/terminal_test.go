package terminal

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-engine/io/wsock"
)

const testToken = "good"

// resizeFrame is what the app sends to resize the terminal.
type resizeFrame struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// testServer is a Server with a shell that is always present and starts fast.
func testServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(testToken)
	s.Shell = "/bin/sh"
	return s
}

func serveTerminal(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func hostPort(t *testing.T, srvURL string) string {
	t.Helper()
	return strings.TrimPrefix(srvURL, "http://")
}

func handshakeRequest(token, query string) string {
	return "GET /terminal" + query + " HTTP/1.1\r\n" +
		"Host: engine\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Bearer " + token + "\r\n\r\n"
}

// dialTerminal completes the handshake and returns the connection and a reader
// positioned right after the 101.
func dialTerminal(t *testing.T, srvURL, token, query string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", hostPort(t, srvURL))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(handshakeRequest(token, query))); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	statusChecked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the handshake response: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if !statusChecked {
			statusChecked = true
			if !strings.HasPrefix(line, "HTTP/1.1 101") {
				t.Fatalf("handshake answered %q, want 101", strings.TrimSpace(line))
			}
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, br
}

// dialStatus does the handshake and returns only the status line, for the cases
// where the handshake is meant to fail. It closes the connection either way, so
// a 101 here does not hold a terminal slot.
func dialStatus(t *testing.T, srvURL, token, query string) string {
	t.Helper()
	conn, err := net.Dial("tcp", hostPort(t, srvURL))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(handshakeRequest(token, query))); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

func readFrame(t *testing.T, conn net.Conn, br *bufio.Reader, within time.Duration) (byte, []byte, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	return wsock.ReadFrame(br)
}

// nextTextFrame reads frames until a text frame arrives and returns it
// unmarshalled. Binary frames on the way are terminal output and are ignored.
func nextTextFrame(t *testing.T, conn net.Conn, br *bufio.Reader, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		op, payload, err := readFrame(t, conn, br, within)
		if err != nil {
			t.Fatalf("no text frame within %s: %v", within, err)
		}
		if op != wsock.OpText {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("text frame %q is not JSON: %v", payload, err)
		}
		return m
	}
	t.Fatalf("no text frame within %s", within)
	return nil
}

// readOutput reads binary frames until they contain marker and returns
// everything read. The command that produces marker must be one whose output
// cannot appear in the echo of what was typed, or this proves nothing.
func readOutput(t *testing.T, conn net.Conn, br *bufio.Reader, marker string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var out strings.Builder
	for {
		if strings.Contains(out.String(), marker) {
			return out.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the output within %s; got %q", marker, within, out.String())
		}
		op, payload, err := readFrame(t, conn, br, within)
		if err != nil {
			t.Fatalf("read frame while waiting for %q (got %q): %v", marker, out.String(), err)
		}
		if op == wsock.OpBinary {
			out.Write(payload)
		}
	}
}

func sendBinary(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	if err := wsock.WriteFrame(conn, wsock.OpBinary, []byte(s)); err != nil {
		t.Fatalf("send binary: %v", err)
	}
}

func sendText(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := wsock.WriteFrame(conn, wsock.OpText, data); err != nil {
		t.Fatalf("send text: %v", err)
	}
}

func TestTerminalRunsAShellAndStreamsBytes(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "?cols=90&rows=25")

	ready := nextTextFrame(t, conn, br, 10*time.Second)
	if ready["type"] != "ready" {
		t.Fatalf("first frame = %v, want ready", ready)
	}
	if ready["cols"] != float64(90) || ready["rows"] != float64(25) {
		t.Fatalf("ready reports %v, want 90x25 from the query", ready)
	}
	if cwd, _ := ready["cwd"].(string); !strings.HasPrefix(cwd, "/") {
		t.Fatalf("ready cwd = %q, want an absolute path", cwd)
	}
	if shell, _ := ready["shell"].(string); !strings.HasPrefix(shell, "/") {
		t.Fatalf("ready shell = %q, want an absolute path", shell)
	}

	// The typed line is echoed back, so wait for output that cannot come from
	// the echo: %s in the input becomes 42 only when the shell ran it.
	sendBinary(t, conn, "printf 'RAN-%s\\n' 42\n")
	readOutput(t, conn, br, "RAN-42", 10*time.Second)
}

func TestTerminalExitFrameCarriesTheCode(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	sendBinary(t, conn, "exit 3\n")
	f := nextTextFrame(t, conn, br, 10*time.Second)
	if f["type"] != "exit" || f["code"] != float64(3) {
		t.Fatalf("frame = %v, want exit with code 3", f)
	}
}

func TestTerminalResizeChangesTheWindow(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	sendText(t, conn, resizeFrame{Type: "resize", Cols: 120, Rows: 40})
	sendBinary(t, conn, "stty size\n")
	readOutput(t, conn, br, "40 120", 10*time.Second)
}

func TestTerminalRejectsBadToken(t *testing.T) {
	srv := serveTerminal(t, testServer(t))

	resp, err := http.Get(srv.URL + "/terminal")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request without a token = %d, want 401", resp.StatusCode)
	}
	if line := dialStatus(t, srv.URL, "wrong", ""); !strings.Contains(line, "401") {
		t.Fatalf("handshake with a wrong token answered %q, want 401", line)
	}
}

func TestTerminalRejectsPlainRequest(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	req, err := http.NewRequest("GET", srv.URL+"/terminal", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET without upgrade headers = %d, want 400", resp.StatusCode)
	}
}

func TestTerminalLimitReturns503(t *testing.T) {
	s := testServer(t)
	s.Limit = 1
	srv := serveTerminal(t, s)

	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	if line := dialStatus(t, srv.URL, testToken, ""); !strings.Contains(line, "503") {
		t.Fatalf("second terminal answered %q, want 503", line)
	}

	// Closing the first one must give the slot back, in the defer, so 503s and
	// dropped sockets both release it.
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		line := dialStatus(t, srv.URL, testToken, "")
		if strings.Contains(line, "101") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was never released: %q", line)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTerminalPingsWhileIdle(t *testing.T) {
	s := testServer(t)
	s.PingEvery = 20 * time.Millisecond
	srv := serveTerminal(t, s)

	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, payload, err := readFrame(t, conn, br, 5*time.Second)
		if err != nil {
			t.Fatalf("no ping within 5s: %v", err)
		}
		if op == wsock.OpPing {
			if len(payload) != 0 {
				t.Fatalf("ping payload = %q, want empty", payload)
			}
			return
		}
	}
	t.Fatalf("no ping within 5s")
}

func TestTerminalClampsResize(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	sendText(t, conn, resizeFrame{Type: "resize", Cols: 0, Rows: 100000})
	sendBinary(t, conn, "printf 'STILL-%s\\n' 1\n")
	readOutput(t, conn, br, "STILL-1", 10*time.Second)
}

func TestTerminalIgnoresPartialResize(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	sendText(t, conn, resizeFrame{Type: "resize", Cols: 10, Rows: 50})
	sendBinary(t, conn, "stty size\n")
	readOutput(t, conn, br, "50 10", 10*time.Second)

	// A frame naming only one dimension must change nothing: defaulting the
	// other to 80x24 would silently resize a terminal the app did not ask to
	// resize. The contract's frame always carries both.
	sendText(t, conn, map[string]any{"type": "resize", "cols": 20})
	sendBinary(t, conn, "printf 'SIZE-%s\\n' \"$(stty size)\"\n")
	readOutput(t, conn, br, "SIZE-50 10", 10*time.Second)
}

func TestTerminalErrorFrameWhenShellMissing(t *testing.T) {
	s := NewServer(testToken)
	s.Shell = "/nonexistent/x"
	srv := serveTerminal(t, s)

	conn, br := dialTerminal(t, srv.URL, testToken, "")
	f := nextTextFrame(t, conn, br, 10*time.Second)
	if f["type"] != "error" {
		t.Fatalf("frame = %v, want error", f)
	}
	if msg, _ := f["error"].(string); msg == "" {
		t.Fatalf("error frame carries no message: %v", f)
	}
	if _, _, err := readFrame(t, conn, br, 5*time.Second); err == nil {
		t.Fatalf("the socket is still open after the error frame")
	}
}

func TestTerminalSurvivesAFlood(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn, br, 10*time.Second)

	sendBinary(t, conn, "yes FLOOD | head -n 200000\n")
	deadline := time.Now().Add(20 * time.Second)
	got := 0
	for got < 900_000 && time.Now().Before(deadline) {
		op, payload, err := readFrame(t, conn, br, 5*time.Second)
		if err != nil {
			break
		}
		if op == wsock.OpBinary {
			got += len(payload)
		}
	}
	if got == 0 {
		t.Fatalf("the flood produced no output at all")
	}

	// The flood must not have wedged the server: a fresh terminal still works.
	conn2, br2 := dialTerminal(t, srv.URL, testToken, "")
	_ = nextTextFrame(t, conn2, br2, 10*time.Second)
	sendBinary(t, conn2, "printf 'AGAIN-%s\\n' 2\n")
	readOutput(t, conn2, br2, "AGAIN-2", 10*time.Second)
}

func TestTerminalTwoAtOnce(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	connA, brA := dialTerminal(t, srv.URL, testToken, "?cols=100&rows=30")
	connB, brB := dialTerminal(t, srv.URL, testToken, "?cols=100&rows=30")

	if readyA := nextTextFrame(t, connA, brA, 10*time.Second); readyA["type"] != "ready" {
		t.Fatalf("terminal A first frame = %v, want ready", readyA)
	}
	if readyB := nextTextFrame(t, connB, brB, 10*time.Second); readyB["type"] != "ready" {
		t.Fatalf("terminal B first frame = %v, want ready", readyB)
	}

	sendBinary(t, connA, "printf 'ONLY-A-%s\\n' 1\n")
	sendBinary(t, connB, "printf 'ONLY-B-%s\\n' 2\n")
	outA := readOutput(t, connA, brA, "ONLY-A-1", 10*time.Second)
	outB := readOutput(t, connB, brB, "ONLY-B-2", 10*time.Second)
	if strings.Contains(outA, "ONLY-B") {
		t.Fatalf("terminal A saw terminal B's output: %q", outA)
	}
	if strings.Contains(outB, "ONLY-A") {
		t.Fatalf("terminal B saw terminal A's output: %q", outB)
	}
}

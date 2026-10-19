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

	"aixodia-engine/io/wsock"
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

// A client that attaches to an existing shell must be able to change its size.
// The shell outlives the link, so a rotation or a keyboard change while the
// phone was offline would otherwise leave it drawing for the old window.
func TestTerminalReattachAppliesTheNewSize(t *testing.T) {
	srv := serveTerminal(t, testServer(t))

	conn, br := dialTerminal(t, srv.URL, testToken, "?id=sized&cols=90&rows=25")
	ready := nextTextFrame(t, conn, br, 10*time.Second)
	if ready["cols"] != float64(90) || ready["rows"] != float64(25) {
		t.Fatalf("first ready = %v, want 90x25", ready)
	}
	sendBinary(t, conn, "stty size\n")
	readOutput(t, conn, br, "25 90", 10*time.Second)
	conn.Close()

	conn2, br2 := dialTerminal(t, srv.URL, testToken, "?id=sized&cols=120&rows=40")
	ready2 := nextTextFrame(t, conn2, br2, 10*time.Second)
	if ready2["cols"] != float64(120) || ready2["rows"] != float64(40) {
		t.Fatalf("reattached ready = %v, want 120x40", ready2)
	}
	sendBinary(t, conn2, "stty size\n")
	readOutput(t, conn2, br2, "40 120", 10*time.Second)
}

// Every attach replays from RIS (ESC c), so a client that still holds the
// previous screen resets before the tail is drawn instead of painting on top
// of it.
func TestTerminalReattachReplaysFromAReset(t *testing.T) {
	srv := serveTerminal(t, testServer(t))
	conn, br := dialTerminal(t, srv.URL, testToken, "?id=replay&cols=90&rows=25")
	_ = nextTextFrame(t, conn, br, 10*time.Second)
	sendBinary(t, conn, "printf 'KEEP-%s\\n' 77\n")
	readOutput(t, conn, br, "KEEP-77", 10*time.Second)
	conn.Close()

	conn2, br2 := dialTerminal(t, srv.URL, testToken, "?id=replay&cols=90&rows=25")
	_ = nextTextFrame(t, conn2, br2, 10*time.Second)
	op, payload, err := readFrame(t, conn2, br2, 10*time.Second)
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	if op != wsock.OpBinary {
		t.Fatalf("replay op = %d, want binary", op)
	}
	if !strings.HasPrefix(string(payload), "\x1bc") {
		t.Fatalf("replay does not start with RIS: %q", payload[:min(len(payload), 24)])
	}
	if !strings.Contains(string(payload), "KEEP-77") {
		t.Fatalf("replay does not carry the shell's tail: %q", payload)
	}
}

func TestTrimRingCutsAtAParserBoundary(t *testing.T) {
	// 9 bytes, cut at index 3 lands in the middle of ESC[31m.
	ring := []byte("AB\x1b[31mCD")
	if got := string(trimRing(ring, 6)); got != "\x1b[31mCD" {
		t.Fatalf("trimRing = %q, want the whole sequence from its ESC", got)
	}
	// A cut inside a three-byte Thai character must not leave its tail behind.
	thai := "ab\u0e2acd" // a b E0 B8 AA c d
	if got := string(trimRing([]byte(thai), 4)); got != "cd" {
		t.Fatalf("trimRing = %q, want the tail after the split character", got)
	}
	// Under the cap, nothing moves.
	if got := string(trimRing([]byte("short"), 64)); got != "short" {
		t.Fatalf("trimRing short = %q", got)
	}
}

func TestAcquireSessionKeepsSizeWhenUnknown(t *testing.T) {
	s := testServer(t)
	sess, _, err := s.acquireSession("keep", 100, 30, termDir(), "/bin/sh")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = sess.pty.Close() })

	// Zero is "the client has not been laid out yet": keep 100x30.
	same, _, err := s.acquireSession("keep", 0, 0, termDir(), "/bin/sh")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if same.cols != 100 || same.rows != 30 {
		t.Fatalf("unknown size resized the shell to %dx%d, want 100x30", same.cols, same.rows)
	}

	// A real size does apply.
	changed, _, err := s.acquireSession("keep", 120, 40, termDir(), "/bin/sh")
	if err != nil {
		t.Fatalf("re-acquire sized: %v", err)
	}
	if changed.cols != 120 || changed.rows != 40 {
		t.Fatalf("shell size = %dx%d, want 120x40", changed.cols, changed.rows)
	}
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

	// Ending the shell (not just the connection) must free the slot: the
	// session stays alive across disconnects, so only a dead session releases it.
	sendBinary(t, conn, "exit 0\n")
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
	_ = conn.Close()
}

// A full engine must still let a client back into the shell it already has:
// reattaching needs no slot. The slot check ran before the id lookup, so the
// last working tab failed to reopen exactly when every slot was taken.
func TestTerminalReattachAtFullHouseIsAllowed(t *testing.T) {
	s := testServer(t)
	s.Limit = 1
	srv := serveTerminal(t, s)

	conn, br := dialTerminal(t, srv.URL, testToken, "?id=full")
	_ = nextTextFrame(t, conn, br, 10*time.Second)
	defer conn.Close()

	conn2, br2 := dialTerminal(t, srv.URL, testToken, "?id=full")
	ready := nextTextFrame(t, conn2, br2, 10*time.Second)
	if ready["id"] != "full" {
		t.Fatalf("reattach ready = %v, want id full", ready)
	}
	_ = conn2.Close()
}

// "Close session" on the phone ends the shell. Hanging up alone left it — and
// its slot — running, so a few closes filled the engine and every new terminal
// answered "too many terminals".
func TestTerminalCloseControlEndsTheShell(t *testing.T) {
	s := testServer(t)
	s.Limit = 1
	srv := serveTerminal(t, s)

	conn, br := dialTerminal(t, srv.URL, testToken, "?id=close-me")
	_ = nextTextFrame(t, conn, br, 10*time.Second)
	if err := wsock.WriteFrame(conn, wsock.OpText, []byte(`{"type":"close"}`)); err != nil {
		t.Fatalf("send close: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, still := s.sess["close-me"]
		s.mu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session stayed after the close frame")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The slot is free now: a different id can open.
	conn2, br2 := dialTerminal(t, srv.URL, testToken, "?id=fresh")
	_ = nextTextFrame(t, conn2, br2, 10*time.Second)
	_ = conn2.Close()
	_ = conn.Close()
}

// A shell no client has attached to for the idle window is abandoned, not
// reattachable, and it must give its slot up instead of failing forever.
func TestIdleShellsAreReapedToMakeRoom(t *testing.T) {
	s := testServer(t)
	s.Limit = 1
	s.IdleAfter = 10 * time.Millisecond

	idle, _, err := s.acquireSession("idle", 80, 24, termDir(), "/bin/sh")
	if err != nil {
		t.Fatalf("acquire idle: %v", err)
	}
	// No subscriber, and its clock is long past the idle window. Backdating it
	// avoids racing the prompt the shell writes when it starts.
	idle.mu.Lock()
	idle.lastAct = time.Now().Add(-time.Hour)
	idle.mu.Unlock()

	next, _, err := s.acquireSession("fresh", 80, 24, termDir(), "/bin/sh")
	if err != nil {
		t.Fatalf("acquire after idle: %v", err)
	}
	t.Cleanup(func() { _ = next.pty.Close() })

	s.mu.Lock()
	_, old := s.sess["idle"]
	s.mu.Unlock()
	if old {
		t.Fatal("an idle shell was not reaped")
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

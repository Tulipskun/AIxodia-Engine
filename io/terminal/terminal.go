package terminal

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"ai-engine/io/wsock"
)

const (
	// defaultLimit is how many terminals may be open at once. Past it the
	// engine answers 503: one phone asking for more connections than this is a
	// mistake, not a workload, and every shell is a real process on the host.
	defaultLimit = 8
	// defaultPingEvery keeps an idle terminal alive across Cloudflare's edge,
	// which drops a WebSocket that has said nothing for about 100 seconds.
	defaultPingEvery = 30 * time.Second
	// readChunk is one read from the pty and so one frame to the phone. The app
	// is told to keep its input frames at or under this size too.
	readChunk = 32 << 10
	// minSize and maxSize bound a terminal size. A client asking for zero
	// columns or a million rows gets clamped rather than handed to the kernel.
	minSize = 1
	maxSize = 1000
)

// Server answers /terminal: every accepted WebSocket gets its own pty and its
// own shell.
type Server struct {
	token string

	mu   sync.Mutex
	open int

	// Limit, PingEvery and Shell exist so tests can shorten or swap things.
	// The zero value means the default above.
	Limit     int
	PingEvery time.Duration
	Shell     string
}

// NewServer returns a /terminal server that accepts token as its bearer token.
func NewServer(token string) *Server {
	return &Server{token: token}
}

func (s *Server) limit() int {
	if s.Limit > 0 {
		return s.Limit
	}
	return defaultLimit
}

func (s *Server) pingEvery() time.Duration {
	if s.PingEvery > 0 {
		return s.PingEvery
	}
	return defaultPingEvery
}

// shell is the program a terminal runs: what was configured, else bash when the
// image has it, else sh.
func (s *Server) shell() string {
	if s.Shell != "" {
		return s.Shell
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// acquire takes a connection slot, or reports that the engine is at its limit.
func (s *Server) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open >= s.limit() {
		return false
	}
	s.open++
	return true
}

// release gives a slot back. It runs in a defer, so a 503 that never got a
// socket, a dropped socket and a clean close all return exactly one slot.
func (s *Server) release() {
	s.mu.Lock()
	s.open--
	s.mu.Unlock()
}

// Handler serves GET /terminal. It checks the bearer token itself: the
// catch-all pattern wrapped in RequireToken never sees this path, because a
// more specific pattern wins.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wsock.BearerOK(r, s.token) {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		if !wsock.IsUpgrade(r) {
			http.Error(w, "websocket upgrade required", http.StatusBadRequest)
			return
		}
		if !s.acquire() {
			http.Error(w, "too many terminals", http.StatusServiceUnavailable)
			return
		}
		defer s.release()

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
		s.serve(conn, br, r)
	})
}

// terminalConn is one open terminal: the socket, the pty behind it, and the
// lock that serialises writes, because the pty pump, the ping ticker and the
// exit notice all write to the same socket, and interleaved frames would
// corrupt the stream.
type terminalConn struct {
	conn net.Conn
	p    *PTY
	mu   sync.Mutex
}

func (c *terminalConn) sendBinary(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsock.WriteFrame(c.conn, wsock.OpBinary, b)
}

func (c *terminalConn) sendText(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsock.WriteFrame(c.conn, wsock.OpText, data)
}

func (c *terminalConn) sendPing() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsock.WriteFrame(c.conn, wsock.OpPing, nil)
}

func (c *terminalConn) sendPong(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsock.WriteFrame(c.conn, wsock.OpPong, payload)
}

func (c *terminalConn) sendClose() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsock.WriteFrame(c.conn, wsock.OpClose, nil)
}

// The frames of the app contract. Text frames are control; terminal bytes never
// travel in one.
type readyFrame struct {
	Type  string `json:"type"`
	Shell string `json:"shell"`
	Cwd   string `json:"cwd"`
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
}

type exitFrame struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

type errorFrame struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

// serve runs one terminal until one of its two ends is gone.
func (s *Server) serve(conn net.Conn, br *bufio.Reader, r *http.Request) {
	cols, rows := wantedSize(r)
	dir := termDir()
	shell := s.shell()

	// A login shell so the image's profiles run and give the phone the PATH,
	// conda and CUDA setup the user would get by hand. If a profile cd's, that
	// is the shell's business, exactly as in a real terminal.
	p, err := StartPTY(shell, []string{"-l"}, cols, rows, dir)
	if err != nil {
		c := &terminalConn{conn: conn}
		_ = c.sendText(errorFrame{Type: "error", Error: err.Error()})
		log.Printf("terminal %s: cannot start %s: %v", r.RemoteAddr, shell, err)
		conn.Close()
		return
	}

	c := &terminalConn{conn: conn, p: p}
	if err := c.sendText(readyFrame{Type: "ready", Shell: shell, Cwd: dir, Cols: cols, Rows: rows}); err != nil {
		p.Close()
		conn.Close()
		return
	}
	log.Printf("terminal %s: opened pid=%d %s %dx%d cwd=%s", r.RemoteAddr, p.cmd.Process.Pid, shell, cols, rows, dir)

	var once sync.Once
	stop := make(chan struct{})
	// shutdown ends the terminal exactly once, whoever gets there first. It
	// hangs the pty up (the kernel signals the shell and the kill covers
	// children that ignore SIGHUP), which also unblocks the pump; it tells the
	// phone the exit code when the shell was the one that ended; and it closes
	// the socket, which unblocks the reader. No terminal content is ever logged.
	shutdown := func(shellEnded bool) {
		once.Do(func() {
			close(stop)
			p.Close()
			if shellEnded {
				_ = c.sendText(exitFrame{Type: "exit", Code: p.Wait()})
			}
			conn.Close()
			log.Printf("terminal %s: closed pid=%d", r.RemoteAddr, p.cmd.Process.Pid)
		})
	}

	go func() {
		ticker := time.NewTicker(s.pingEvery())
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := c.sendPing(); err != nil {
					shutdown(false)
					return
				}
			}
		}
	}()

	go func() {
		buf := make([]byte, readChunk)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				if err := c.sendBinary(buf[:n]); err != nil {
					shutdown(false)
					return
				}
			}
			if err != nil {
				// The pty hung up, which means the shell ended. If the phone
				// left first, stop is already closed and nobody is listening.
				select {
				case <-stop:
					return
				default:
				}
				shutdown(true)
				return
			}
		}
	}()

	for {
		op, payload, err := wsock.ReadFrame(br)
		if err != nil {
			break
		}
		// Any opcode not handled here — a continuation frame's 0x0, a pong —
		// is dropped. The contract is one frame per message, so a continued
		// message is not something the app sends; see docs/superpowers/specs/
		// 2026-10-18-engine-terminal-design.md §3.
		switch op {
		case wsock.OpBinary:
			if _, err := p.Write(payload); err != nil {
				shutdown(false)
				return
			}
		case wsock.OpText:
			if cols, rows, ok := parseResize(payload); ok {
				if err := p.Resize(cols, rows); err != nil {
					log.Printf("terminal %s: resize to %dx%d: %v", r.RemoteAddr, cols, rows, err)
				}
			}
		case wsock.OpPing:
			if err := c.sendPong(payload); err != nil {
				shutdown(false)
				return
			}
		case wsock.OpClose:
			_ = c.sendClose()
			shutdown(false)
			return
		}
	}
	shutdown(false)
}

// parseResize reads the one control frame the app sends. Anything else — an
// unknown type, a frame that is not JSON, a frame that names only one of the
// two dimensions — is ignored: a bad control frame must not end a working
// terminal, and it must not silently resize it either, which is what filling a
// missing dimension with a default would do.
func parseResize(payload []byte) (cols, rows uint16, ok bool) {
	var in struct {
		Type string `json:"type"`
		Cols *int   `json:"cols"`
		Rows *int   `json:"rows"`
	}
	if err := json.Unmarshal(payload, &in); err != nil || in.Type != "resize" || in.Cols == nil || in.Rows == nil {
		return 0, 0, false
	}
	return uint16(clamp(*in.Cols)), uint16(clamp(*in.Rows)), true
}

// wantedSize is the starting size: what the app asked for in the query,
// clamped, else 80x24.
func wantedSize(r *http.Request) (cols, rows uint16) {
	colsInt, rowsInt := 80, 24
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil {
		colsInt = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil {
		rowsInt = v
	}
	return uint16(clamp(colsInt)), uint16(clamp(rowsInt))
}

func clamp(v int) int {
	if v < minSize {
		return minSize
	}
	if v > maxSize {
		return maxSize
	}
	return v
}

// termDir is where a new shell starts: the notebook's working directory when
// the engine is running inside Kaggle, else wherever the engine itself was
// started, which is $AIXODIA_HOME because the launcher cd's there. localdb
// chooses its database path by the same rule.
func termDir() string {
	// Prefer the user's home directory so a new shell starts where the
	// operator's files actually live; fall back to the notebook working dir and
	// the engine's own dir for the other deployments.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			return home
		}
	}
	if st, err := os.Stat("/kaggle/working"); err == nil && st.IsDir() {
		return "/kaggle/working"
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

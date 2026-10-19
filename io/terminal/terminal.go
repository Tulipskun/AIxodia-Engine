package terminal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"aixodia-engine/io/wsock"
)

const (
	defaultLimit     = 16
	defaultPingEvery = 30 * time.Second
	readChunk        = 32 << 10
	minSize          = 1
	maxSize          = 1000
	// How long a shell with no attached client stays alive before being reaped.
	idleReapAfter = 30 * time.Minute
	// Ring buffer of pty output kept for reattach.
	ringCap = 64 << 10
)

// risReset is RIS (ESC c): everything back to how a fresh terminal looks.
// Every attach replays onto it so a returning client never draws on top of the
// screen it already had.
var risReset = []byte("\x1bc")

type Server struct {
	token     string
	mu        sync.Mutex
	sess      map[string]*session
	Limit     int
	PingEvery time.Duration
	// IdleAfter overrides how long an unattached shell is kept before the
	// reaper ends it. Zero means idleReapAfter.
	IdleAfter time.Duration
	Shell     string
}

type session struct {
	id         string
	pty        *PTY
	mu         sync.Mutex
	subs       map[*subscriber]struct{}
	ring       []byte
	modes      *modeTracker
	cols, rows uint16
	dead       bool
	exitCode   int
	hasExit    bool
	createdAt  time.Time
	lastAct    time.Time
	stop       chan struct{}
	once       sync.Once
}

type subscriber struct {
	conn net.Conn
	mu   sync.Mutex
}

func NewServer(token string) *Server {
	return &Server{token: token, sess: map[string]*session{}}
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
func (s *Server) idleAfter() time.Duration {
	if s.IdleAfter > 0 {
		return s.IdleAfter
	}
	return idleReapAfter
}
func (s *Server) shell() string {
	if s.Shell != "" {
		return s.Shell
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// acquireSession reuses a live session id or starts a shell for it. The bool is
// whether the program should be nudged to repaint: true on a reattach at the
// size it already had, where the kernel sends no SIGWINCH and a full-screen
// program would otherwise leave the client with a partial frame from the ring.
func (s *Server) acquireSession(id string, cols, rows uint16, dir, sh string) (*session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sess[id]; ok && !sess.dead {
		sess.lastAct = time.Now()
		kick := false
		// A returning client may be a different size than the shell was left
		// at (rotation, keyboard, another phone). Apply it before the replay:
		// the pty sends SIGWINCH, a full-screen program repaints, and the bytes
		// that follow are laid out for the window about to draw them. A zero
		// dimension means the client does not know its size yet, so the shell
		// is left as it was rather than shrunk to the placeholder.
		sess.mu.Lock()
		if cols > 0 && rows > 0 && (cols != sess.cols || rows != sess.rows) {
			sess.cols, sess.rows = cols, rows
			if !sess.dead {
				_ = sess.pty.Resize(cols, rows)
			}
		} else if !sess.dead && sess.modes.altScreen() {
			kick = true
		}
		sess.mu.Unlock()
		return sess, kick, nil
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if len(s.sess) >= s.limit() {
		s.reapLocked(false)
		if len(s.sess) >= s.limit() {
			// Still full: no client has been attached to the oldest shells for
			// the idle window, so they are abandoned rather than reattachable.
			s.reapLocked(true)
		}
		if len(s.sess) >= s.limit() {
			return nil, false, errors.New("too many terminals")
		}
	}
	p, err := StartPTY(sh, []string{"-l"}, cols, rows, dir)
	if err != nil {
		return nil, false, err
	}
	sess := &session{
		id: id, pty: p, subs: map[*subscriber]struct{}{},
		modes: newModeTracker(),
		cols:  cols, rows: rows, createdAt: time.Now(), lastAct: time.Now(),
		stop: make(chan struct{}),
	}
	s.sess[id] = sess
	go sess.readLoop()
	go sess.pingLoop(s.pingEvery())
	return sess, false, nil
}

// reapLocked drops finished sessions, and — when makeRoom is set — sessions no
// client has attached to for idleReapAfter. Without the second case a session
// the phone stopped tracking (a closed tab whose close never arrived, an app
// that was cleared) keeps its shell and its slot for the life of the engine.
// The caller holds s.mu; the session lock is taken underneath, never above.
func (s *Server) reapLocked(makeRoom bool) {
	now := time.Now()
	for id, sess := range s.sess {
		sess.mu.Lock()
		reap := sess.dead ||
			(makeRoom && len(sess.subs) == 0 && now.Sub(sess.lastAct) > s.idleAfter())
		sess.mu.Unlock()
		if !reap {
			continue
		}
		delete(s.sess, id)
		// A dead session's master is still open until this closes it; the shell
		// itself is already gone.
		_ = sess.pty.Close()
	}
}

// drop ends a session for good: it leaves the map and its shell is hung up.
// Used when the phone says the session is closed, not merely that the window is.
func (s *Server) drop(id string, sess *session) {
	s.mu.Lock()
	if s.sess[id] == sess {
		delete(s.sess, id)
	}
	s.mu.Unlock()
	_ = sess.pty.Close()
}

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
		cols, rows := wantedSize(r)
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			id = newID()
		}
		// Slot check before the handshake so a full engine answers 503 as an HTTP
		// status instead of upgrading first. Shell-start failures are handled
		// after the upgrade (serve), where they can carry an error frame.
		//
		// Reattaching to a shell that is already running needs no slot. Counting
		// it as one made the last working tab fail to reopen on a full engine,
		// which is exactly when the phone retries.
		s.mu.Lock()
		s.reapLocked(false)
		_, existing := s.sess[id]
		full := !existing && len(s.sess) >= s.limit()
		s.mu.Unlock()
		if full {
			http.Error(w, "too many terminals", http.StatusServiceUnavailable)
			return
		}
		dir := termDir()
		sh := s.shell()
		conn, br, err := wsock.Accept(w, r)
		if err != nil {
			return
		}
		sess, kick, err := s.acquireSession(id, cols, rows, dir, sh)
		if err != nil {
			_ = wsock.WriteFrame(conn, wsock.OpText, mustJSON(map[string]any{"type": "error", "error": err.Error()}))
			conn.Close()
			return
		}
		s.serve(conn, br, r, sess, id, cols, rows, dir, sh, kick)
	})
}

// Attach serves an already-established connection as terminal session id,
// creating the session when the id is new. The relay uses it for terminal
// streams that arrive over its single socket; the handshake and bearer check
// belong to the relay's socket, not here.
func (s *Server) Attach(conn net.Conn, id string, cols, rows int) error {
	if id == "" {
		id = newID()
	}
	// Zero is a real value here: the app may reattach before its view has been
	// laid out, and it should then keep the shell's current size rather than
	// force the 80x24 placeholder onto a running program. A new shell still
	// gets the defaults inside acquireSession.
	c, r := uint16(0), uint16(0)
	if cols > 0 {
		c = uint16(clamp(cols))
	}
	if rows > 0 {
		r = uint16(clamp(rows))
	}
	dir, sh := termDir(), s.shell()
	// acquireSession is where the slot is checked, and it exempts an id that is
	// already alive: reattaching does not need a new one.
	sess, kick, err := s.acquireSession(id, c, r, dir, sh)
	if err != nil {
		return err
	}
	// The ready frame must name the size the shell actually has; when the
	// client sent zero that is the size it was already running at.
	sess.mu.Lock()
	effC, effR := sess.cols, sess.rows
	sess.mu.Unlock()
	go s.serve(conn, bufio.NewReader(conn), nil, sess, id, effC, effR, dir, sh, kick)
	return nil
}

func (s *Server) serve(conn net.Conn, br *bufio.Reader, r *http.Request, sess *session, id string, cols, rows uint16, dir, sh string, kick bool) {
	// attach
	sub := &subscriber{conn: conn}
	// The replay and the registration have to be one ordered step. Registering
	// first lets a chunk read off the pty land between the ring snapshot and
	// the replay: the client draws it, then RIS and the older ring paint over
	// it, and that chunk is gone until the program writes again. Holding the
	// session lock across both keeps readLoop from taking its subscriber
	// snapshot in that window, so anything newer than the ring is delivered
	// after it.
	sess.mu.Lock()
	ring := append([]byte(nil), sess.ring...)
	// replay ring buffer, preceded by the modes the program set before the
	// ring began (mouse reporting, alternate screen, cursor keys)
	modes := sess.modes.replay()
	// send ready
	_ = wsock.WriteFrame(conn, wsock.OpText, mustJSON(map[string]any{
		"type": "ready", "shell": sh, "cwd": dir, "cols": cols, "rows": rows, "id": id,
	}))
	// Replay onto a known state. A client may be reattaching to a screen it
	// already has — after a dropped link, or after the engine restarted under
	// an open window — so drawing the tail on top of the old screen duplicates
	// prompts and paints a full-screen app's frame over the previous one. RIS
	// resets the client first; the modes then re-enter the alternate screen
	// before the tail is drawn into it. It is all one binary frame so the
	// order reset -> modes -> output cannot be split by the transport.
	replay := make([]byte, 0, len(risReset)+len(modes)+len(ring))
	replay = append(replay, risReset...)
	replay = append(replay, modes...)
	replay = append(replay, ring...)
	if len(replay) > 0 {
		_ = wsock.WriteFrame(conn, wsock.OpBinary, replay)
	}
	sess.subs[sub] = struct{}{}
	sess.mu.Unlock()
	// A reattach at the size the shell already had leaves the kernel nothing to
	// signal, so a full-screen program would keep only the slice of its last
	// frame that fits in the ring. Ask it to redraw the whole screen.
	if kick {
		_ = sess.pty.Kick()
	}
	// read loop
	for {
		op, payload, err := wsock.ReadFrame(br)
		if err != nil {
			break
		}
		switch op {
		case wsock.OpBinary:
			sess.mu.Lock()
			if !sess.dead {
				_, _ = sess.pty.Write(payload)
			}
			sess.mu.Unlock()
		case wsock.OpText:
			var in struct {
				Type string `json:"type"`
				Cols *int   `json:"cols"`
				Rows *int   `json:"rows"`
			}
			if json.Unmarshal(payload, &in) != nil {
				break
			}
			switch in.Type {
			case "resize":
				if in.Cols == nil || in.Rows == nil {
					break
				}
				c, rr := uint16(clamp(*in.Cols)), uint16(clamp(*in.Rows))
				sess.mu.Lock()
				sess.cols, sess.rows = c, rr
				if !sess.dead {
					_ = sess.pty.Resize(c, rr)
				}
				sess.mu.Unlock()
			case "close":
				// The phone closed the session, not just the window: end the
				// shell and free its slot now rather than leaving it for the
				// idle reaper.
				sess.detach(sub)
				s.drop(id, sess)
				conn.Close()
				return
			}
		case wsock.OpPing:
			sub.mu.Lock()
			_ = wsock.WriteFrame(conn, wsock.OpPong, payload)
			sub.mu.Unlock()
		case wsock.OpClose:
			sub.mu.Lock()
			_ = wsock.WriteFrame(conn, wsock.OpClose, nil)
			sub.mu.Unlock()
			sess.detach(sub)
			conn.Close()
			return
		}
	}
	sess.detach(sub)
	conn.Close()
}

func (sess *session) readLoop() {
	buf := make([]byte, readChunk)
	for {
		n, err := sess.pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			sess.mu.Lock()
			sess.modes.scan(chunk)
			sess.ring = trimRing(append(sess.ring, chunk...), ringCap)
			sess.lastAct = time.Now()
			subs := make([]*subscriber, 0, len(sess.subs))
			for sub := range sess.subs {
				subs = append(subs, sub)
			}
			sess.mu.Unlock()
			for _, sub := range subs {
				sub.mu.Lock()
				_ = wsock.WriteFrame(sub.conn, wsock.OpBinary, chunk)
				sub.mu.Unlock()
			}
		}
		if err != nil {
			code := sess.pty.Wait()
			sess.mu.Lock()
			if !sess.dead {
				sess.dead = true
				sess.hasExit = true
				sess.exitCode = code
				close(sess.stop)
			}
			subs := make([]*subscriber, 0, len(sess.subs))
			for sub := range sess.subs {
				subs = append(subs, sub)
			}
			sess.mu.Unlock()
			for _, sub := range subs {
				data, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
				sub.mu.Lock()
				_ = wsock.WriteFrame(sub.conn, wsock.OpText, data)
				sub.mu.Unlock()
			}
			return
		}
	}
}

func (sess *session) pingLoop(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-sess.stop:
			return
		case <-ticker.C:
			sess.mu.Lock()
			subs := make([]*subscriber, 0, len(sess.subs))
			for sub := range sess.subs {
				subs = append(subs, sub)
			}
			sess.mu.Unlock()
			for _, sub := range subs {
				sub.mu.Lock()
				err := wsock.WriteFrame(sub.conn, wsock.OpPing, nil)
				sub.mu.Unlock()
				if err != nil {
					sess.detach(sub)
				}
			}
		}
	}
}

func (sess *session) detach(sub *subscriber) {
	sess.mu.Lock()
	delete(sess.subs, sub)
	sess.lastAct = time.Now()
	sess.mu.Unlock()
}

func newID() string {
	return fmt.Sprintf("t-%d", time.Now().UnixNano())
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func wantedSize(r *http.Request) (uint16, uint16) {
	cols, rows := 80, 24
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil {
		cols = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil {
		rows = v
	}
	return uint16(clamp(cols)), uint16(clamp(rows))
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

func termDir() string {
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

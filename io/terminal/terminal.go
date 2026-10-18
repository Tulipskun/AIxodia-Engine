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

type Server struct {
	token     string
	mu        sync.Mutex
	sess      map[string]*session
	Limit     int
	PingEvery time.Duration
	Shell     string
}

type session struct {
	id         string
	pty        *PTY
	mu         sync.Mutex
	subs       map[*subscriber]struct{}
	ring       []byte
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
func (s *Server) shell() string {
	if s.Shell != "" {
		return s.Shell
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func (s *Server) acquireSession(id string, cols, rows uint16, dir, sh string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sess[id]; ok && !sess.dead {
		sess.lastAct = time.Now()
		return sess, nil
	}
	if len(s.sess) >= s.limit() {
		// reap dead ones
		for k, v := range s.sess {
			if v.dead {
				delete(s.sess, k)
			}
		}
		if len(s.sess) >= s.limit() {
			return nil, errors.New("too many terminals")
		}
	}
	p, err := StartPTY(sh, []string{"-l"}, cols, rows, dir)
	if err != nil {
		return nil, err
	}
	sess := &session{
		id: id, pty: p, subs: map[*subscriber]struct{}{},
		cols: cols, rows: rows, createdAt: time.Now(), lastAct: time.Now(),
		stop: make(chan struct{}),
	}
	s.sess[id] = sess
	go sess.readLoop()
	go sess.pingLoop(s.pingEvery())
	return sess, nil
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
		// Slot check before the handshake so a full engine answers 503 as an HTTP
		// status instead of upgrading first. Shell-start failures are handled
		// after the upgrade (serve), where they can carry an error frame.
		s.mu.Lock()
		for k, v := range s.sess {
			if v.dead {
				delete(s.sess, k)
			}
		}
		full := len(s.sess) >= s.limit()
		s.mu.Unlock()
		if full {
			http.Error(w, "too many terminals", http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			id = newID()
		}
		dir := termDir()
		sh := s.shell()
		conn, br, err := wsock.Accept(w, r)
		if err != nil {
			return
		}
		sess, err := s.acquireSession(id, cols, rows, dir, sh)
		if err != nil {
			_ = wsock.WriteFrame(conn, wsock.OpText, mustJSON(map[string]any{"type": "error", "error": err.Error()}))
			conn.Close()
			return
		}
		s.serve(conn, br, r, sess, id, cols, rows, dir, sh)
	})
}

func (s *Server) serve(conn net.Conn, br *bufio.Reader, r *http.Request, sess *session, id string, cols, rows uint16, dir, sh string) {
	// attach
	sub := &subscriber{conn: conn}
	sess.mu.Lock()
	sess.subs[sub] = struct{}{}
	// replay ring buffer
	ring := append([]byte(nil), sess.ring...)
	sess.mu.Unlock()
	// send ready
	_ = wsock.WriteFrame(conn, wsock.OpText, mustJSON(map[string]any{
		"type": "ready", "shell": sh, "cwd": dir, "cols": cols, "rows": rows, "id": id,
	}))
	if len(ring) > 0 {
		_ = wsock.WriteFrame(conn, wsock.OpBinary, ring)
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
			// resize
			var in struct {
				Type string `json:"type"`
				Cols *int   `json:"cols"`
				Rows *int   `json:"rows"`
			}
			if json.Unmarshal(payload, &in) == nil && in.Type == "resize" && in.Cols != nil && in.Rows != nil {
				c, rr := uint16(clamp(*in.Cols)), uint16(clamp(*in.Rows))
				sess.mu.Lock()
				sess.cols, sess.rows = c, rr
				if !sess.dead {
					_ = sess.pty.Resize(c, rr)
				}
				sess.mu.Unlock()
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
			sess.ring = append(sess.ring, chunk...)
			if len(sess.ring) > ringCap {
				sess.ring = sess.ring[len(sess.ring)-ringCap:]
			}
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

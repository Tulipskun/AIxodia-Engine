package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"aixodia-engine/io/wsock"
)

// Handler answers phone sockets opened directly against the engine, which is
// what a quick tunnel exposes. The frames are the same ones the Cloudflare
// relay forwards — chat, terminal streams and rpc requests — so neither the
// hub, the terminal nor the HTTP mux knows which transport carried them.
func Handler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, cfg)
	})
}

func serve(w http.ResponseWriter, r *http.Request, cfg Config) {
	// The tunnel is public, so the token is the only door: it is checked before
	// the upgrade, exactly as the relay's own handshake is.
	if !wsock.BearerOK(r, cfg.Token) {
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

	c := wsock.NewServerConn(conn, br)
	// The phone pings every ten seconds on its own socket layer; a minute
	// without any frame means the link is gone and its streams with it.
	c.SetLiveness(90 * time.Second)

	s := newSession(cfg, c)
	defer s.close()

	// The phone's client stays in CONNECTING until it hears that an engine is
	// behind the socket, and only then sends its hello. On a direct socket the
	// engine is trivially present, so the note is the first frame.
	if err := c.WriteMessage(wsock.OpText, []byte(`{"ctl":"presence","engine":true}`)); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go s.ping(ctx)

	for {
		op, payload, err := c.ReadMessage()
		if err != nil {
			return
		}
		switch op {
		case wsock.OpText:
			// The client introduces itself with {"role":"phone"} before any
			// protocol frame. The relay consumed that note on its way through;
			// here it reaches the hub, which would read the empty body as a
			// blank message and answer with an error the user would see.
			if isRoleNote(payload) {
				continue
			}
			if err := s.text(payload); err != nil {
				return
			}
		case wsock.OpBinary:
			s.binary(payload)
		}
	}
}

// isRoleNote reports whether the frame is the client's role handshake and
// nothing else.
func isRoleNote(payload []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	_, role := m["role"]
	_, ch := m["ch"]
	_, typ := m["type"]
	return role && !ch && !typ
}

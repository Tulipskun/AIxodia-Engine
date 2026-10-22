# TASK-P2P: optional WebRTC DataChannel for the terminal (engine side)

Prerequisite: the relay cleanup in TASK.md is merged. Start from that main.
The old branch `feat/terminal-webrtc-p2p` is built on the deleted relay socket: do not merge it. Copy only the ideas from its `io/terminal/webrtc.go` (pion setup, wrapping the DataChannel in a net.Pipe with wsock framing so the existing attach code is reused).

Goal: after a /terminal WebSocket is attached, the phone may open a direct DataChannel to the engine and move that terminal onto it. WebSocket stays the default and the fallback. Terminal only: no chat, no runs, no HTTP.

Design (do not deviate without a written reason)
- Signaling rides the existing /terminal WebSocket as new text control frames next to resize/ready: `{"type":"rtc-offer","sdp":...}` from the phone; `{"type":"rtc-answer","sdp":...}` or `{"type":"rtc-error","message":...}` back. Non-trickle ICE: wait for gathering, capped at 3s.
- ICE: STUN only. Default `stun:stun.cloudflare.com:3478`, override with AIXODIA_STUN. No TURN anywhere. If no direct path exists the connection fails and the phone stays on WebSocket.
- DataChannel label `term`, ordered, reliable. Messages use the same framing as the /terminal WebSocket (text = control JSON, binary = raw PTY bytes). The existing attach path is reused unchanged.
- Switching is a reattach. Once the DataChannel is open the phone attaches the same shell id over it and gets the normal replay (reset + modes + tmux redraw), then closes its WebSocket attach. Falling back to WebSocket is the ordinary reconnect through /terminal. Do not invent a lossless hand-over protocol.
- Keepalive: answer `{"type":"ping","t":N}` over the DataChannel with `{"type":"pong","t":N}`. The phone uses it to measure RTT and detect a dead path.
- Input: never replay or resend keystrokes after a path failure (a command could run twice). The screen is repainted from the shell, so losing a few keystrokes is acceptable.
- Auth: accept signaling only on an authenticated (ticketed) /terminal socket. One PeerConnection per socket, offer size <= 64 KiB. A PeerConnection with no open DataChannel 20s after the offer is closed; after that its lifetime is the DataChannel's (the WebSocket may close once the phone has switched).
- Flag: AIXODIA_P2P=1 enables it. Default off: an offer is answered with `rtc-error` "disabled" and nothing else changes.
- Dependency: add github.com/pion/webrtc/v4 (pure Go; keep CGO_ENABLED=0 builds for amd64 and arm64). Update the "third-party code" line in AGENTS.md. Report the binary size delta.
- Logging: once per connection log the chosen candidate-pair type (host/srflx/relay). Never log SDP or IP addresses.
- Tests: a Go test with an in-process pion client against the handler over loopback: offer/answer, DataChannel attach gets the replay, ping/pong, rejected when disabled, oversize offer rejected, orphan PeerConnection reaped. With AIXODIA_P2P unset the behaviour must be unchanged.

Rules
- Read AGENTS.md first. Never pkill the engine.
- Before each commit: go build ./... && go vet ./... && gofmt -l . && go test ./...
- Work on branch `p2p-terminal`. Commit only, do NOT push to main (a push to main releases and self-updates the live engine).
- README: document the flag, the protocol frames, and that there is no TURN.

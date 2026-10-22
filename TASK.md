# TASK: remove dead relay code (aixodia-engine)

Decision (owner, 2026-10-23): the terminal stays on the plain WebSocket (/terminal). No WebRTC, no P2P, no TURN. Do not add pion/webrtc or any rtc-* signaling frames. TASK-P2P.md and the branch feat/terminal-webrtc-p2p have been deleted. If you have a local branch p2p-terminal or a checkout of feat/terminal-webrtc-p2p, delete it.

Context: since commit b727ece the engine no longer dials the Cloudflare relay Worker. It serves /room itself behind a cloudflared quick tunnel. Chat is POST /v1/runs + SSE, terminal is /terminal with a ticket. The old relay path is believed dead but must be proven.

Rules
- Read AGENTS.md first. Never pkill the engine.
- Before each commit: go build ./... && go vet ./... && gofmt -l . && go test ./...
- One commit per item. Commit only, do NOT push (a push to main triggers a release and a self-update of the live engine).
- Report what you deleted, what you kept and why.

Steps
1. Prove each candidate unused (grep imports and callers). Delete only what is proven dead.
2. Candidates: relay/ (Worker + DO); the outbound relay client in io/relay (relay.Run, Config.URL/Token, live_test.go); the /room and /ws routes in main.go (and rpcMux if only /room used it); io/mobile and the client side of io/wsock if nothing else uses them; their tests.
3. Keep: /terminal, /v1/*, run/, tunnel.go, D1 daemon/url + heartbeat discovery, wsock server code used by /terminal.
4. Fix stale docs: README says the terminal uses WebView xterm.js (it is native) and mentions the old /ws relay; update the relay and /room lines in AGENTS.md. Keep the README sentence that says there is no P2P/WebRTC/TURN path.
5. Remove /ws and /room in a separate, last code commit: an old APK still installed would stop connecting.
6. Do not touch the deployed Worker; the owner deletes it with `npx wrangler delete --name aixodia-relay`.
7. Delete the remote branch feat/terminal-webrtc-p2p (the owner's Claude session could not: its git proxy refuses ref deletion): `git push origin --delete feat/terminal-webrtc-p2p`. It holds only the abandoned WebRTC work (tip 0ba071e). If you cannot push, say so in your report.
8. Final commit: delete this TASK.md.

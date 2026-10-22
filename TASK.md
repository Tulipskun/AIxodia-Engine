# TASK: remove dead relay code

Context: since commit b727ece the engine no longer dials the Cloudflare relay Worker. It serves /room itself behind a cloudflared quick tunnel. Chat is POST /v1/runs + SSE, terminal is /terminal with a ticket. The old relay path is believed dead but must be proven.

Rules
- Read AGENTS.md first. Never pkill the engine.
- Before each commit: go build ./... && go vet ./... && gofmt -l . && go test ./...
- One commit per item. Commit only, do NOT push (a push to main triggers a release and a self-update of the live engine).
- Report what you kept and why.

Steps
1. Prove each candidate unused (grep imports and callers). Delete only what is proven dead.
2. Candidates: relay/ (Worker + DO); the outbound relay client in io/relay (relay.Run, Config.URL/Token, live_test.go); the /room and /ws routes in main.go (and rpcMux if only /room used it); io/mobile and the client side of io/wsock if nothing else uses them; their tests.
3. Keep: /terminal, /v1/*, run/, tunnel.go, D1 daemon/url + heartbeat discovery, wsock server code used by /terminal.
4. Fix stale docs: README says the terminal uses WebView xterm.js (it is native) and mentions the old /ws relay; update the relay and /room lines in AGENTS.md.
5. Remove /ws and /room in a separate, last commit: an old APK still installed would stop connecting.
6. Do not touch the deployed Worker; the owner deletes it.

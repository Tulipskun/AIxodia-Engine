# Engine terminal over WebSocket — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve the engine host's shell to the AIxodia app over a WebSocket on the engine's existing tunnel — `wss://<tunnel>/terminal` with the app's existing bearer token — so the app can offer a Termux-like terminal.

**Architecture:** Four pieces: a shared `io/wsock` package (the RFC 6455 handshake and framing that `io/mobile` already implements, lifted so both endpoints use one copy), a Linux pty runner in `io/terminal`, the `/terminal` WebSocket handler that pumps bytes between the socket and that pty, and one route in `main.go`. No new dependencies: stdlib plus the `golang.org/x/sys/unix` already in `go.mod`.

**Tech Stack:** Go 1.22.2 (repo floor; CI pins it), stdlib, `golang.org/x/sys/unix` (pty ioctls), the repo's own hand-rolled WebSocket framing.

**Spec:** `docs/superpowers/specs/2026-10-18-engine-terminal-design.md`

## Global Constraints

- **No new dependencies.** stdlib + `golang.org/x/sys/unix` only (the spec verifies the four pty ioctls and the three `Ioctl*` helpers exist in x/sys v0.48.0).
- **Linux only.** Kaggle and `install.sh` targets are Linux; no build tags, but say so in a comment where the pty is set up.
- **Do not change existing behaviour:** `/ws` chat framing, every `/api` and `/v1` route, the tunnel, snapshots, `POST /restart`, and the log DB stay exactly as they are. The chat path is only re-pointed at the shared framing package.
- **Auth is the existing bearer token** (`CF_TOKEN`) in the handshake, checked with a constant-time compare. No password, no `/etc/ssh`, no new daemon.
- **Version bump:** `io/gateway/gateway.go:35` `v1.138` → `v1.139` (one bump per release, repo convention). A push to `main` publishes the rolling `engine-latest` release (`.github/workflows/release.yml`).
- **Style:** comments in English and explaining *why*; user-visible log lines in English; commit messages in English.
- **Every task ends with** `go build ./... && go vet ./... && gofmt -l . && go test ./...` green (prepend `go clean -testcache`).
- Scope is the engine only. The app side (`aixodia-app`) is the human partner's.

## Review Focus

Inputs and conditions the spec implies that no test would catch on its own. Each has a test in the owning task.

1. **An idle terminal** — a shell sitting at a prompt for minutes gets its WebSocket closed by Cloudflare's edge (~100 s). The engine must ping every 30 s (Task 3).
2. **Garbage resize** — `{"type":"resize","cols":0,"rows":100000}` (or a negative value) must not panic, must not break the shell, and must be clamped (Task 3).
3. **A client that vanishes mid-command** — no close frame, just a dropped socket. The shell and its children must die, leaving no orphan (Tasks 2 and 3).
4. **A shell that floods** — `yes` / `find /` producing far more output than the tunnel can carry must not deadlock the reader, the ping, or the close path (Task 3).
5. **Two terminals at once** — separate ptys, no cross-talk, and the cap counts connections not sockets-per-connection (Task 3).

---

### Task 1: `io/wsock` — the WebSocket primitives, shared

Lift the handshake and framing out of `io/mobile` unchanged, so `/ws` and `/terminal` use one copy. Behaviour must not change: `io/mobile`'s existing tests are the gate.

**Files:**
- Create: `io/wsock/wsock.go`
- Create: `io/wsock/wsock_test.go`
- Modify: `io/mobile/ws.go` (delete the moved code, call the new package)
- Modify: `io/mobile/ws_test.go:41,123,228` (`readFrame` → `wsock.ReadFrame`)

**Interfaces:**
- Produces, package `wsock`:
  - `const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"`, `const MaxFrame = 1 << 20`
  - `const (OpText = 0x1; OpBinary = 0x2; OpClose = 0x8; OpPing = 0x9; OpPong = 0xA)`
  - `var ErrNotUpgrade = errors.New("websocket upgrade required")`
  - `func BearerOK(r *http.Request, token string) bool` — true only for a constant-time match and a non-empty token
  - `func IsUpgrade(r *http.Request) bool`
  - `func Accept(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error)` — `ErrNotUpgrade` when `IsUpgrade` is false, a plain error when the writer cannot hijack; otherwise writes the 101 and returns the hijacked connection and reader
  - `func ReadFrame(r *bufio.Reader) (byte, []byte, error)`
  - `func WriteFrame(w io.Writer, op byte, payload []byte) error`
- Consumes: nothing.

- [ ] **Step 1: Write the failing tests** in `io/wsock/wsock_test.go`

`TestBearerOK` — table over `(header, token, want)`: `"Bearer good"`/`"good"` → true; `"Bearer bad"`/`"good"` → false; `""`/`"good"` → false; `"Bearer good"`/`""` → false; `"good"`/`"good"` (no scheme) → false.

`TestAcceptRejectsPlainRequest` — `httptest.NewRequest("GET", "/terminal", nil)` into an `httptest.NewRecorder`; `errors.Is(err, ErrNotUpgrade)` and the recorder must be untouched (`500`-class body absent, code still 200).

`TestAcceptWritesTheHandshake` — a real `net.Pipe`-free path: run `httptest.NewServer(http.HandlerFunc(...Accept...))` and dial it with `net.Dial` + the four handshake headers using `Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==`; read the response line and assert `Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=` (the RFC 6455 example value).

`TestReadWriteFrameRoundTrip` — `WriteFrame` into a `bytes.Buffer` for `OpBinary` payloads of length 5, 200, and 70000, then `wsock.ReadFrame(bufio.NewReader(&buf))` returns the same op and payload; also assert a masked client frame (mask bit set, payload XORed with the mask) reads back unmasked.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./io/wsock/ -v`
Expected: FAIL — build error, `no required module provides package aixodia-engine/io/wsock` (the package does not exist yet).

- [ ] **Step 3: Create `io/wsock/wsock.go`** with the code moved verbatim from `io/mobile/ws.go`: `wsGUID` → `GUID`, `maxFrame` → `MaxFrame`, `readFrame` → `ReadFrame`, `writeFrame` → `WriteFrame`, plus the opcode block, `ErrNotUpgrade`, and the two new helpers. `IsUpgrade` is the four-header test currently inside `Hub.Handler` (`io/mobile/ws.go:165-171`); `Accept` is the hijack plus 101 write from `io/mobile/ws.go:172-187`; `BearerOK` is `io/mobile/ws.go:160-164`.

- [ ] **Step 4: Run the wsock tests to verify they pass**

Run: `go test ./io/wsock/ -v`
Expected: PASS.

- [ ] **Step 5: Point `io/mobile` at the package**

In `io/mobile/ws.go`: `Hub.Handler` becomes `BearerOK` → 401, hijack via `wsock.Accept` (`ErrNotUpgrade` → 400, other error → 500), and `p.send`/the `serve` loop use `wsock.WriteFrame`/`wsock.ReadFrame` with `wsock.OpText`/`wsock.OpClose`/`wsock.OpPing`. Delete the moved functions and the now-unused imports (`crypto/sha1`, `crypto/subtle`, `encoding/base64`, `encoding/binary`, `errors`, `io`; keep `bufio`, `net`). Keep the package doc comment true.

- [ ] **Step 6: Update the three frame-reading call sites in the mobile test**

`io/mobile/ws_test.go:41,123,228`: `readFrame(r)` → `wsock.ReadFrame(r)`; add the import.

- [ ] **Step 7: Run the whole toolchain — the mobile tests are the regression gate**

Run: `go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...`
Expected: build/vet clean, `gofmt -l .` prints nothing, all packages PASS (notably `ok aixodia-engine/io/mobile`).

- [ ] **Step 8: Commit**

```bash
git add io/wsock io/mobile/ws.go io/mobile/ws_test.go
git commit -m "wsock: one copy of the websocket handshake and framing"
```

---

### Task 2: `io/terminal` — a shell on a pty

**Files:**
- Create: `io/terminal/pty.go`
- Create: `io/terminal/pty_test.go`

**Interfaces:**
- Produces, package `terminal`:
  - `func StartPTY(shell string, cols, rows uint16, dir string) (*PTY, error)`
  - `func (p *PTY) Read(b []byte) (int, error)` / `func (p *PTY) Write(b []byte) (int, error)` — master side
  - `func (p *PTY) Resize(cols, rows uint16) error`
  - `func (p *PTY) Wait() int` — blocks until the shell exits; the exit code, or `-1` when it died from a signal
  - `func (p *PTY) Close() error` — closes the master, then kills the shell's process group if it is still alive
- Consumes: nothing.

- [ ] **Step 1: Write the failing tests** in `io/terminal/pty_test.go`

Use `/bin/sh` (not bash) and a helper `readUntil(t *testing.T, p *PTY, marker string, within time.Duration) string` that reads into a buffer until the marker appears or the deadline passes, failing the test on timeout. `StartPTY` failure is the only way any of these block forever, so every `Wait`/`Close` assertion must run inside a bounded goroutine-and-select.

- `TestPTYShellSeesWindowSize` — `StartPTY("/bin/sh", 100, 30, t.TempDir())`; write `stty size; echo SIZE-DONE\nexit 7\n`; `readUntil(..., "SIZE-DONE", 5*time.Second)` contains `30 100`; `Wait() == 7`.
- `TestPTYResizeSignalsTheShell` — start at 80x24, `Resize(120, 40)`, then `stty size` read back is `40 120`.
- `TestPTYShellDeathIsASignal` — write `kill -TERM $$\n`; `Wait() == -1`.
- `TestPTYCloseKillsTheShell` — write `exec sleep 300\n`; `Close()`; `Wait()` returns within 5 s (a `-1` or `143` code is fine, the point is that it returned).
- `TestPTYMissingShellFails` — `StartPTY("/nonexistent/sh", 80, 24, "")` returns a non-nil error and a nil `*PTY`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./io/terminal/ -v`
Expected: FAIL — `StartPTY` undefined (package does not exist yet).

- [ ] **Step 3: Implement `io/terminal/pty.go`**

Open `/dev/ptmx` with `os.OpenFile`, unlock it with `unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)`, read the number with `unix.IoctlGetInt(fd, unix.TIOCGPTN)`, then open `/dev/pts/N` `O_RDWR|O_NOCTTY`. Start the shell with `exec.Command(shell)` (add `-l` in the handler's caller-supplied path, not here), `cmd.Stdin = cmd.Stdout = cmd.Stderr = slave`, `cmd.Dir = dir` when `dir != ""`, `cmd.Env = append(os.Environ(), "TERM=xterm-256color")`, and `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}` so the slave is the shell's controlling terminal. Set the window size through `unix.IoctlSetWinsize(masterFd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})` — `Resize` is the same call, and the kernel raises `SIGWINCH` itself. `Wait` runs `cmd.Wait()` once (guard with `sync.Once`), mapping `*exec.ExitError` to `ExitCode()` and a `Signaled()` wait status to `-1`. `Close` closes the master (which hangs the pty up), then `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)` when `cmd.ProcessState == nil` (the process group, because `Setsid` made the shell a group leader), then reaps. Comment that this is Linux-only and why the group kill is needed (children of a shell that ignores SIGHUP). Close the slave in the parent after `cmd.Start`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./io/terminal/ -v`
Expected: PASS for all five tests.

- [ ] **Step 5: Confirm the pty code did not disturb anything else**

Run: `go build ./... && go vet ./... && gofmt -l .`
Expected: clean, `gofmt` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add io/terminal/pty.go io/terminal/pty_test.go
git commit -m "terminal: a login shell on a pty, sized and cleaned up"
```

---

### Task 3: `io/terminal` — the `/terminal` WebSocket handler

**Files:**
- Create: `io/terminal/terminal.go`
- Create: `io/terminal/terminal_test.go`

**Interfaces:**
- Produces, package `terminal`:
  - `func NewServer(token string) *Server`
  - `func (s *Server) Handler() http.Handler`
  - test knobs on `Server` (zero value = production default): `Limit int` (concurrent terminals, default 8), `PingEvery time.Duration` (default 30 s), `Shell string` (default `/bin/bash` when it exists, else `/bin/sh`)
- Consumes: `wsock.BearerOK`/`IsUpgrade`/`Accept`/`ReadFrame`/`WriteFrame` (Task 1), `terminal.StartPTY`/`PTY.Read`/`Write`/`Resize`/`Wait`/`Close` (Task 2).

Wire format, exactly as the spec fixes it: binary frames carry terminal bytes both ways; text frames from the client are `{"type":"resize","cols":N,"rows":N}`; text frames to the client are `{"type":"ready","shell":...,"cwd":...,"cols":N,"rows":N}`, then one of `{"type":"exit","code":N}` or `{"type":"error","error":"..."}` before the close. Optional `?cols=&rows=` starts the pty at that size.

- [ ] **Step 1: Write the failing tests** in `io/terminal/terminal_test.go`

Add a test client helper: `dialTerminal(t, srvURL, token string, query string) (net.Conn, *bufio.Reader)` — TCP-dial the `httptest.NewServer` URL, write the four handshake headers plus `Authorization: Bearer <token>`, read until the blank line after the 101, return the connection and a `bufio.Reader` for `wsock.ReadFrame`. A second helper `sendBinary(t, conn, s)` / `sendText(t, conn, json)` uses `wsock.WriteFrame` (the server accepts unmasked frames), and `nextText(t, r, within)` reads frames until a text frame arrives, failing a binary frame in between is not an error for the byte-level tests.

- `TestTerminalRunsAShellAndStreamsBytes` — `?cols=90&rows=25`; first text frame is `ready` with `cols:90`, `rows:25` and a non-empty absolute `cwd`; send binary `echo hi\n`; read until the output contains `hi`.
- `TestTerminalExitFrameCarriesTheCode` — send binary `exit 3\n`; next text frame is `{"type":"exit","code":3}`.
- `TestTerminalResizeChangesTheWindow` — send `{"type":"resize","cols":120,"rows":40}` then binary `stty size\n`; output contains `40 120`.
- `TestTerminalRejectsBadToken` — status 401 with no upgrade (`http.Get` against the server) and also 401 for a dial with `Bearer wrong`.
- `TestTerminalRejectsPlainRequest` — `GET /terminal` with the right token but no upgrade headers → 400.
- `TestTerminalLimitReturns503` — `Server.Limit = 1`, hold one connection open, a second dial is answered `HTTP/1.1 503`.
- `TestTerminalPingsWhileIdle` — `Server.PingEvery = 20 * time.Millisecond`; within two seconds `wsock.ReadFrame` yields `wsock.OpPing`.
- `TestTerminalClampsResize` — send `{"type":"resize","cols":0,"rows":100000}`, then `echo ok\n`; output still contains `ok` and the connection is alive.
- `TestTerminalErrorFrameWhenShellMissing` — `Server.Shell = "/nonexistent/x"`; first text frame is `{"type":"error",...}` and then the socket closes (a following read returns an error).
- `TestTerminalSurvivesAFlood` — send binary `yes FLOOD | head -n 200000\n`; drain frames until a text frame arrives or the connection ends, with a 20 s deadline; the test must not hang and the engine must still answer a following `echo second\n` in a new terminal (open a second connection, assert `second`).
- `TestTerminalTwoAtOnce` — two connections, distinct `ready` frames, first sends `echo first\n`, second sends `echo second\n`; each sees only its own marker.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./io/terminal/ -run Terminal -v`
Expected: FAIL — `NewServer` undefined.

- [ ] **Step 3: Implement `io/terminal/terminal.go`**

Handler order: `wsock.BearerOK` → 401; `wsock.IsUpgrade` → 400; `Server.Limit` reached → 503; then `wsock.Accept`. Parse `cols`/`rows` from the query (`strconv.Atoi`, default 80x24, clamp to 1..1000). Resolve `dir := termDir()` — `/kaggle/working` when `os.Stat` says it is a directory, else `os.Getwd()` — and the shell (explicit `Shell`, else `/bin/bash` when present, else `/bin/sh`) with `-l` appended. `StartPTY` failure → `{"type":"error","error":...}` then close (400/401/503 stay pre-upgrade). Send `ready`. Then three goroutines sharing a `sync.Mutex`-guarded `writeFrame` (the mobile `peer.send` pattern, because the pty pump, the ping ticker and the exit notice all write):

- the socket reader: binary → `PTY.Write`; text → unmarshal `{"type":"resize",...}` and `PTY.Resize` after clamping; `OpClose` → return; `OpPing` → answer `OpPong` with the same payload;
- the pty pump: read into a 32 KiB buffer (`const readChunk = 32 << 10`), write each chunk as one binary frame, stop when the read fails;
- the ping ticker: `time.NewTicker`, write `OpPing` with an empty payload every interval, stop on write error.

The first of reader/pump to finish tears the whole thing down: close the master, `PTY.Close()` to kill the group, one `exit` frame carrying `PTY.Wait()` when the shell exited on its own (not when the client just went away), close the connection. Log one line per terminal opened and closed with the remote address and the shell pid — never the bytes. Keep the concurrency note that `Limit` is decremented in a `defer` so 503's and dropped sockets both release it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./io/terminal/ -run Terminal -v`
Expected: PASS for all ten tests.

- [ ] **Step 5: Run the whole toolchain**

Run: `go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...`
Expected: all PASS, `gofmt -l .` empty.

- [ ] **Step 6: Commit**

```bash
git add io/terminal/terminal.go io/terminal/terminal_test.go
git commit -m "terminal: the /terminal websocket, byte for byte"
```

---

### Task 4: wire the route and bump the version

**Files:**
- Modify: `main.go` (the mux block around `main.go:105-110`)
- Modify: `io/gateway/gateway.go:35`
- Modify: `go.mod` (only if `go mod tidy` moves `golang.org/x/sys` out of the indirect block)

**Interfaces:**
- Consumes: `terminal.NewServer(token).Handler()` (Task 3).
- Produces: the live `/terminal` route and `gateway.Version == "v1.139"`.

- [ ] **Step 1: Add the route**

In `main.go`, next to `mux.Handle("/ws", ...)`: `mux.Handle("/terminal", terminal.NewServer(token).Handler())`, with a one-line comment saying why it asks for the token itself (a specific pattern beats the `"/"` catch-all, so `RequireToken` never sees it).

- [ ] **Step 2: Bump the version** — `io/gateway/gateway.go:35`: `const Version = "v1.139"`.

- [ ] **Step 3: Tidy the module and run the whole toolchain**

```bash
go mod tidy && git diff --stat go.mod go.sum
go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...
```
Expected: `go.mod` at most moves `golang.org/x/sys v0.48.0` to the direct block (no version change, no new module, `go.sum` untouched); everything green.

- [ ] **Step 4: Confirm the route is reachable without a token**

Run: `go run . &` is not needed — the route check happens in Task 5 against the live engine. Here, assert instead that the gateway tests still pass: `go test ./io/gateway/ -count=1`.
Expected: PASS (the mux gained a route; nothing else moved).

- [ ] **Step 5: Commit**

```bash
git add main.go io/gateway/gateway.go go.mod go.sum
git commit -m "engine: v1.139 — /terminal serves the host's shell"
```

---

### Task 5: deploy and verify against the live engine

Operator task (needs the Cloudflare token and the tunnel URL; run it from the repo on `main`). No code changes.

**Files:** none.

- [ ] **Step 1: Push and watch both workflows**

```bash
git push origin main
gh run list --limit 4          # CI + Release both green for the pushed SHA
```
Expected: `verify` and `Release engine` succeed; the release uploads `aixodia-engine` for linux/amd64 and arm64 to `engine-latest`.

- [ ] **Step 2: Restart the session and confirm the new version**

```bash
curl -sS -X POST "$ENGINE/restart" -H "Authorization: Bearer $CF_TOKEN"
# the container restarts, installs engine-latest, and the tunnel comes back (~60 s)
curl -sS "$ENGINE/healthz"      # → {"version":"v1.139",...}
```
Expected: `v1.139`; if the tunnel URL changed, take the new one from D1 (`daemon/url`).

- [ ] **Step 3: Exercise the real endpoint through the tunnel** with a throwaway client at `/tmp/opencode/termcheck/main.go` (its own `go.mod`, no import of `aixodia-engine`, hand-rolled handshake plus masked frames). It takes the tunnel URL + token, opens `/terminal`, and prints frames. Verify, in order:

- the `ready` frame's `shell` runs, `cwd` is an absolute path, `cols`/`rows` match `?cols=100&rows=30`;
- `echo hi` → output contains `hi`; `stty size` → `30 100`;
- `{"type":"resize","cols":140,"rows":45}` then `stty size` → `45 140`;
- `top -b -n 1 | head -3` prints a process table (a real full-screen consumer); `nano --version` runs;
- `exit 3` → `{"type":"exit","code":3}`;
- idle: open a terminal, wait 150 s without sending anything, then send `echo still-here` → still answered (the 30 s ping kept the edge open);
- orphan check: `pgrep -af 'bash -l'` before and after (open a terminal, run it, close the app/socket) — the count comes back and no shell is left behind;
- regression: the same client against `/ws` sends `{"type":"hello"}` and gets `{"kind":"ack"}` (the chat path still works through the shared framing).

- [ ] **Step 4: Check the engine's own log lines**

```bash
curl -sS "$ENGINE/api/logs?limit=100" -H "Authorization: Bearer $CF_TOKEN" | grep -i terminal
```
Expected: one open line and one close line per connection, with remote address and pid, and no terminal content.

- [ ] **Step 5: Report to the human partner** — the tunnel URL, the exact URL/headers/frames the app must use (spec §3), and anything the live run disproved. No commit.

---

## Self-Review

- **Spec coverage:** route + auth (§2 Goals, Task 3/4); pty, shell, cwd rule, `TERM`, resize (§4.2, Tasks 2–3); lifecycle, exit/error frames, ping (§4.3, Tasks 2–3); safety cap and "no content in logs" (§4.4, Task 3); the app contract (§3, produced by Tasks 3–4 and handed over in Task 5); no new dependency (Tasks 2–4); version bump and rolling release (§7, Tasks 4–5); `wsock` split (§8, Task 1); risks list (§9) covered by Review Focus.
- **Step scan:** every step names a file, a signature, or a command plus the output that means it passed. No step is "handle edge cases" or "write tests for the above".
- **Type consistency:** `wsock.ReadFrame`/`WriteFrame`/`Op*`/`BearerOK`/`IsUpgrade`/`Accept`/`ErrNotUpgrade` are used with the same names in Tasks 1, 3; `StartPTY`/`PTY`/`Resize`/`Wait`/`Close` between Tasks 2, 3; `NewServer`/`Handler`/`Limit`/`PingEvery`/`Shell` between Tasks 3, 4.
- **Review Focus:** the five lines map to `TestTerminalPingsWhileIdle`, `TestTerminalClampsResize`, `TestPTYCloseKillsTheShell` + the shutdown step in Task 3, `TestTerminalSurvivesAFlood`, `TestTerminalTwoAtOnce`.
- **Proportion:** the plan is roughly twice the spec, and code blocks are assertions and signatures, not bodies.

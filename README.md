# AIxodia

The AIxodia engine: serves the chat API on `127.0.0.1:8787`, puts a Cloudflare
quick tunnel in front of it, and tells the phone the tunnel URL via D1.

## Install / update

```bash
curl -fsSL https://raw.githubusercontent.com/Tulipskun/aixodia-engine/main/install.sh | sudo bash
```

Installs to `/usr/local/bin`: `aixodia-engine`, `aixodia`, and `cloudflared` (if it
is not already on your PATH). Linux x86_64 and arm64.

Updating is the same command: `aixodia update` installs the published build and
hands it to the running engine, which swaps the binary and carries on under the
same pid. A running build too old to accept the handshake is restarted instead.

## Run

```bash
export CF_TOKEN=xxxxx    # the only secret; used to publish the tunnel url to D1
aixodia
```

`aixodia` starts the engine in the background, waits for the tunnel URL, prints
it, and gives the prompt back.

```bash
aixodia status      # up? at which url
aixodia logs        # tail the engine log
aixodia stop        # shut it down
aixodia update      # install the published build and update in place
```

`aixodia update` does not stop the engine. It sends `SIGUSR1` to the pid the
engine reports on `GET /healthz`, and the engine installs the build and execs
itself: same pid, same arguments, same session. A chat in flight is not cut
short — the engine notes the request and applies the build when the turn ends.
Only a build that predates the handshake (no `update` key on `/healthz`) is
stopped and started, which ends the attached session.

The engine also installs a published build by itself, ten minutes at a time and
only while no chat is in flight (`selfupdate: installing the build published
...` in the log); `aixodia update` is the same swap on demand.

Only local state lives in `~/.aixodia/` (`aixodia.pid`, `aixodia.log`,
`aixodia-logs.db`, `uploads/`). Chat history and settings are in D1; providers
and their API keys are in Workers KV.

## Configuration

Only `CF_TOKEN` is required. Everything else has a working default, and the
usual engine variables (`AIXODIA_MODEL`, `AIXODIA_DB_PATH`, `AIXODIA_UPLOADS`)
are respected when set.

Chat history, settings and the note a session leaves for the one that replaces
it live in **D1** (`chat_sessions`, `chat_turns`, `system_state`). Providers and
their API keys live in **Workers KV**: namespace `aixodia-providers`, one key
`providers` holding the whole list. D1's `system_state` also carries the daemon
discovery keys (`daemon/url`, `daemon/version`, `daemon/heartbeat`), which is how
the phone finds the current tunnel. `CF_TOKEN` therefore needs D1 and Workers KV
read/write.

## Development

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
bash aixodia_test.sh      # launcher
bash install_test.sh      # installer (offline, file:// fixtures)
```

## Terminal transport: WebRTC P2P (planned)

Today the engine dials the Cloudflare Durable Objects room and the phone carries terminal traffic through that shared relay socket. The relay therefore sees terminal traffic for the full lifetime of each shell.

The target is to use Durable Objects for **signaling only** and move terminal data to WebRTC DataChannel:

1. The engine and Android client exchange SDP and ICE candidates through the room.
2. WebRTC ICE attempts a direct path, using STUN for address discovery.
3. Once the DataChannel opens, PTY input/output, resize events, and terminal control messages travel directly between Android and the engine.
4. Use a configurable TURN server only when direct connectivity fails. Keep the current relay terminal path as a fallback during rollout and for clients that do not support P2P.
5. Keep PTY ownership and shell-session lifecycle in the engine. Signaling must never execute commands or carry terminal output after P2P has been established.

Implementation requirements:

- Android: libwebrtc DataChannel transport integrated with TerminalSocket.
- Linux engine: Pion WebRTC peer connection attached to the existing terminal sessions.
- Signaling: authenticated, session-scoped messages; validate message sizes, peer roles, expiry, and session ownership.
- Reliability: reconnect/re-attach to the same PTY session, bounded queues/backpressure, connection state reporting, and relay fallback.
- Security: do not expose the PTY directly to the public network; do not reuse the Cloudflare API token as a peer-facing long-lived signaling credential.

**Status:** design target, not implemented. The current engine still uses io/relay for terminal traffic. Do not describe relay traffic as eliminated until the Android client, engine, signaling Worker, fallback behavior, and end-to-end tests are implemented and verified.


# AIxodia

The AIxodia engine: serves the chat API on `127.0.0.1:8787`, dials out to the
Cloudflare relay, and tells the phone where to find it via D1.

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
the phone finds the relay room. `CF_TOKEN` therefore needs D1 and Workers KV
read/write.

## Development

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
bash aixodia_test.sh      # launcher
bash install_test.sh      # installer (offline, file:// fixtures)
```

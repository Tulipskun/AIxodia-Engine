# AIxodia

The AIxodia engine: serves the chat API on `127.0.0.1:8787`, publishes itself
through a Cloudflare quick tunnel, and tells the phone where to find it via D1.

## Install / update

```bash
curl -fsSL https://raw.githubusercontent.com/Tulipskun/aixodia-engine/main/install.sh | sudo bash
```

Installs to `/usr/local/bin`: `aixodia-engine`, `aixodia`, and `cloudflared` (if it
is not already on your PATH). Linux x86_64 and arm64.

Updating is the same command: it replaces the files. Restart a running engine
afterwards to pick up the new build.

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
aixodia update      # install the published build and restart into it
```

The engine also installs a published build by itself, ten minutes at a time and
only while no chat is in flight (`selfupdate: installing the build published
...` in the log); `aixodia update` is the same download on demand.

State lives in `~/.aixodia/` (`aixodia.pid`, `aixodia.log`, `aixodia.db`,
`uploads/`).

## Configuration

Only `CF_TOKEN` is required. Everything else has a working default, and the
usual engine variables (`AIXODIA_MODEL`, `AIXODIA_DB_PATH`, `AIXODIA_UPLOADS`)
are respected when set.

Everything the engine owns lives in one SQLite file: chat history, providers
with their API keys, settings, and the note a session leaves for the one that
replaces it. D1 holds only the daemon discovery keys (`daemon/url`,
`daemon/version`, `daemon/heartbeat`), which is how the phone finds the
tunnel — no request waits on it.

## Development

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
bash aixodia_test.sh      # launcher
bash install_test.sh      # installer (offline, file:// fixtures)
```

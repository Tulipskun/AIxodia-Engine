# Rust port — milestones

The Go engine is ~24,200 lines across 19 packages. This workspace is the port,
built **beside** the Go code so both can be compared until feature parity.

Nothing here is deployed yet, and `rust/` is not wired into `.github/workflows`
(release still builds the Go binary).

## Verify

```bash
cd rust
cargo build --workspace
cargo test --workspace
cargo fmt --check
cargo clippy --workspace --all-targets   # rustup component add clippy
```

## Status

| Go package | Rust crate | Lines (Go) | Status |
|---|---|---:|---|
| `db/` (D1 REST + Workers KV + `system_state`) | `aixodia-db` | 470 | **ported** |
| `config/` (provider store, built-ins) | `aixodia-config` | 531 | **ported**, 8 tests |
| `provider/` types + `Adapter` | `aixodia-provider::types` | 228 | **ported** |
| `provider/openai/` (chat + Responses API) | `aixodia-provider::openai` | 1,023 | **ported** |
| `provider/opencode/` (Zen contract + ladder) | `aixodia-provider::opencode` | 574 | **ported**, 2 integration tests |
| `tools/` | `aixodia-tools` | 1,453 | declarations only — execution pending |
| `session/` (D1 store, trimming, blocks) | `aixodia-session` | 1,908 | pending |
| `io/gateway/` (HTTP API, chat loop, SSE) | `aixodia-io` | ~3,500 | pending |
| `io/terminal/` (tmux, ring buffer, tickets) | `aixodia-io` | 800 | pending |
| `io/relay/`, `io/wsock/`, `io/mobile/` | `aixodia-io` | ~1,200 | pending |
| `run/` (runs, events, approval gate) | `aixodia-run` | 1,682 | pending |
| `selfupdate/` (handover, exec) | `aixodia-selfupdate` | 695 | pending |
| `imageprep/` | `aixodia-io` or own crate | 370 | pending |
| `goals/` | `aixodia-run` | — | pending |
| `d1log.go` (local SQLite logs) | `aixodia-engine` | — | pending |
| `main.go` + `tunnel.go` + `handover.go` | `aixodia-engine` (bin) | ~1,300 | pending |
| `relay/` (TypeScript Worker + D1) | separate `workers-rs` crate | ~? | pending |

## Decisions taken so far

- **Async everywhere the Go code touches the network.** D1/KV calls are
  `async`; the `config` store takes `Arc<dyn KeyValue>` so tests use an
  in-memory map exactly like the Go fake.
- **Package globals became explicit handles.** `db.token/AccountID/DBID` live in
  one `RwLock<Credentials>` behind a cheap-to-clone `Client`; the `config`
  globals stay globals (they are process-wide by design) behind `OnceLock`.
- **`Emitter` trait instead of `Box<dyn FnMut(&str)>`.** A `&mut dyn FnMut(&str)`
  threaded through several `async` layers needs a higher-ranked bound the
  compiler cannot infer at the call site; a concrete trait with a blanket impl
  for `FnMut` keeps every call site a plain closure.
- **`reqwest::RequestBuilder::header` appends, it does not set.** The opencode
  contract needs `Header.Set` semantics (its `User-Agent` must replace
  `aixodia-engine/1.0`). Headers are therefore assembled into one `HeaderMap`
  and applied with `.headers(..)`; the integration test caught this.
- **The opencode adapter takes its builtin declarations by injection**
  (`Adapter::with_builtin(aixodia_tools::builtin_fn())`) so `provider` never
  depends on `tools`. The port keeps placeholder `shell`/`read` definitions for
  callers that do not inject, matching the Go fallback.

## Next milestones (in order)

1. `aixodia-tools` execution: `shell`, `read`, `edit`, `write`, `current_time`,
   `fetch_url` (+ the SSRF guard from Go's `safePublicDialContext`), approval
   gate names.
2. `aixodia-session`: D1-backed sessions, `AppendTurns` chunking (6 rows /
   96 params), trail + block JSON, `EnsureTurns`, token estimation, the sliding
   window with usage anchoring.
3. `aixodia-run`: runs/events/approval gate, interrupted-run recovery.
4. `aixodia-io`: gateway routes, SSE, model routing, tool loop (12 rounds,
   3 attempts with 750ms/1.5s backoff, key rotation), `/room` socket, terminal
   tickets + tmux + ring buffer.
5. `aixodia-selfupdate` + the binary: `CF_TOKEN` boot sequence, cloudflared
   quick tunnel, heartbeat, `SIGUSR1` handover via `execve`.
6. `relay/` → `workers-rs`, with the same D1 schema and room protocol.
7. Cut over: CI builds Rust, `install.sh` ships it, Go packages deleted.

## Porting rules

- Behaviour first: a port is done when the Go tests for that package pass
  against the Rust implementation, not when the signatures match.
- Keep the wire formats byte-compatible (request bodies, SSE framing, D1 SQL and
  column names, KV JSON shape).
- Never change the Go code to make the port easier; fix the port.

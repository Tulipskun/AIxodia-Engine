# AGENTS

## Verify changes
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...` — the whole toolchain. No CI, no lint config, no Makefile.
- `go test ./io/gateway` is the useful one: it drives the gateway end to end against a fake OpenAI upstream (no network, no D1) and covers model routing, the tool loop, SSE streaming, session persistence, and history merging. Run it after touching `io/gateway`, `provider/`, `session/`, or `tools/`.
- `go test ./provider/opencode` checks the Zen free-tier request contract (headers, forced `stream`, injected tools) against a fake upstream — no network.
- `db` and `config` cannot be unit-tested offline: `db.Verify` must hit api.cloudflare.com first or every call returns "Verify must succeed first".

## Architecture (not obvious from filenames)
- Module `aixodia-engine`, Go 1.22.2, **stdlib only** (no go.sum, no third-party deps).
- `db/` is not a local database — it is a thin HTTP client for the Cloudflare **D1 REST API**. No SQL files, no migrations; schema lives only in D1 and in the column names quoted in code.
  - The query body must be a **JSON object**: `{"sql":"...","params":[...]}`. An array of statements is rejected with error 7400 ("Expected object, received array") — this silently broke the original code.
- Startup (`main.go`): `CF_TOKEN` → `db.Verify` → `config.LoadConfig()` (error logged, process continues) → serve `127.0.0.1:8787` → config reloads every 60s → `cloudflared` quick tunnel → `db.SaveTunnel(url)`.
  - `db.Verify` sets package globals (`token`, `AccountID`, `DBID`); every `db.*` call fails until it runs.
  - D1 target: first account the token can see; database named `aixodia`, else the first one. No env override.
  - `cloudflared` must be on PATH; the public URL is scraped from its stderr (45s timeout).

## HTTP API (`io/gateway`)
- `GET /healthz`, `GET /v1/providers`, `GET /v1/models[?provider=Name]` (fetches upstream `/models`, 60s cache).
- Sessions: `POST /v1/sessions`, `GET /v1/sessions`, `GET|DELETE /v1/sessions/{id}`, `GET /v1/sessions/{id}/turns`. Go 1.22 `ServeMux` patterns with `{id}`.
- Chat: `POST /v1/chat/completions`, OpenAI-compatible, `"stream":true` for SSE.
- **Model routing**: `model` must be `provider/model` (e.g. `NousResearch/inclusionai/ling-3.1-flash`), or give the `provider` field. A bare model name errors with 400 — there is no model→provider index anywhere.
- **Sessions own history**: when `session_id` is set the engine loads turns from D1 and merges the incoming `messages` by common prefix, so clients may send either the full history or only the new turn. Reply, tokens, and duration are appended as turns; the session's `provider`, `model`, and first `title` are written on success.
- Defaults: all builtin tools are offered unless the body has `"tools": []`; context is trimmed to `session.DefaultMaxTokens` (100k estimated tokens); at most 12 tool rounds, and the round that spends the last of them carries a closing instruction (`chat.go: outOfRounds`) so a turn that runs out ends with a result instead of narration; upstream calls get 3 attempts with 750ms/1.5s backoff on retryable errors, keys rotated round-robin per provider; `sub_provider`/`sub_model` is a one-shot fallback while nothing has been streamed yet.
- Upstream failures return 502 `{"error":{"message","type":"upstream_error"}}`; mid-stream failures emit a `data: {"error":...}` event then `[DONE]`.

## Schema in use
- `providers(index, provider, endpoint, keys, adapter, free)` — no `model` column, so the client always names the model.
  - `config.Provider` fields differ from columns; mapping is `config.providerFromRow` (`index`→`ID`, `provider`→`Name`, `endpoint`→`APIURL`). `keys` is a JSON array as text.
- `sessions(...)` and `turns(id, session_id, seq, role, text, model, input_tokens, output_tokens, duration_ms, ...)` — other clients already write here (existing rows with Thai titles), so keep column names and `UNIQUE(session_id, seq)` in mind.
  - Tool steps are stored as `tool_call` / `tool_result` turns (JSON in `text`, see `session/trail.go`) and replayed into the next request. They are written after each tool round, not at the end, so a turn that fails or is cut off still keeps the tools it ran.
  - `turns.block` holds the row's display block as JSON (`session/block.go`: text/reasoning/tool/request/status). The engine owns this vocabulary — each provider round also announces `request` waiting/done events — and the phone renders blocks generically with a plain-card fallback for unknown types, so new kinds of content need no app update. Rows that predate blocks have `block=''` and render through the role.
  - `reasoning_content` (added by `EnsureTurns`) holds what a reasoning model wrote before its answer, so the replayed assistant turn carries it; a provider that serves one refuses a history without it. `reasoning_tokens` is only the count for the footer.
- `tunnel(url)`: `db.SaveTunnel` creates the table, deletes all rows, inserts one.

## db package rules
- Helpers `Query`, `Select`, `Insert`, `Update`, `Delete`, `Where`; identifiers must match `^[A-Za-z_][A-Za-z0-9_]*$`, values always bound as `?`.
- `Update` needs a non-empty `Where`; `Delete` with no `Where` wipes the table. Writes need raw `db.Query` for `ORDER BY` (`session` does this for turns).
- Rows come back as `[]map[string]any`; D1 numbers arrive as `float64`.

## Adapters (`provider/`)
- Shared types and the `Adapter` interface live in package `provider` (`provider/types.go`). They are **not** under `provider/internal/`: Go's `internal` rule would block `session`, `tools`, and `io/gateway` from importing them, and that directory was removed.
- Two adapters: **openai** (streaming, tools, usage — also used by `agentrouter`, `tokenharbor`, `cavoti`, `NousResearch`) and **opencode** (wraps openai, see contract below). `provider/anthropic` and `provider/gemini` are still empty placeholders — a D1 row using those adapter names fails with `unknown adapter`.
- Outbound HTTP must send `User-Agent: aixodia-engine/1.0` (`provider.UserAgent`). Cloudflare returns a 403 HTML challenge for Go's default `Go-http-client/1.1` — this was a real outage on NousResearch.
- `openai.Adapter.HeaderFunc` is called per request and its entries overwrite the standard headers — that is the only hook the opencode adapter uses.

### OpenCode Zen free-tier contract (`provider/opencode`)
Verified by live probes against `https://opencode.ai/zen/v1/chat/completions` on 2026-10-07. All four conditions are required; missing any one gives `403 FreeTierError "can only be used from within OpenCode"` (426 `UpgradeRequired` if the UA version is below 1.18.0, 403 error 1010 if UA is absent):
1. `User-Agent: opencode/<version>` — any version ≥ 1.18.0 works (`opencode/1.18.31` is what we send); `aixodia-engine/1.0` and `Go-http-client/1.1` are rejected. This overrides `provider.UserAgent`.
2. `x-opencode-session: ses_<12 hex><14 base62>` — required and format-checked (all-zero timestamps rejected). We keep one per adapter instance and send **no** `x-opencode-request` header at all: the gate reads that header as a request the client never sent and answers 403 FreeTierError. Measured 2026-10-15 by replaying the engine's own captured request — with the header 403, remove only that header and the identical request is 200. Everything the request-id headers would have carried is already in the message and session headers. `x-opencode-client: cli`, `x-opencode-project: global` are sent for fidelity but are not independently required (the real project hash was tried and changed nothing).
3. Body must declare tools — `shell` **and** `read` at minimum, which the adapter injects when the request has none. `shell` is the command tool's name: the real OpenCode client (v2) sends `shell` (v1's `bash` was renamed to it), and it is the name the gate reads. `bash` was measured to pass too (2026-10-16), but the engine offers `shell` only so it matches the real client. A tool-less body is refused by the gate: a plain `instructions`-only title request the client makes is accepted, but the same body with a generic system prompt is 403. With the tools present the body is accepted with or without a system prompt, so the contract injection in the ladder must not be dropped.
4. Body must have `"stream": true` — a non-stream request is rejected even with correct headers. The adapter therefore always calls the openai stream path; `Complete` buffers the deltas into `Response.Content` (usage and `finish_reason` arrive on the final chunk).
5. Body `tools` must include **both** `shell` **and** `read` — the adapter injects the real builtin defs for the missing ones (never stub shells). Extra tools (our builtins) alongside are accepted.
6. Go to the Responses API first: chat completions was refused on 2026-10-14 for the same tool declarations the Responses API accepted. The ladder in `provider/opencode` tries `/responses` first and keeps chat completions as a fallback. Endpoint path: `/inference/v1/responses` (verified working 2026-10-15); `/inference/openai/v1/...` per the console docs answers identically. The stale `zen/v1` in some D1 rows is not the live path.

Also: Zen intermittently returns `429 "Upstream request failed: Endpoint is unavailable."` (their capacity, not our request) — the gateway's 3-attempt retry covers it, same stance as NousResearch.

## Live provider state (verified 2026-10-07, all from this host)
- **NousResearch** works: `inclusionai/ling-3.1-flash` returns real completions, but with frequent transient `429 temporarily at capacity upstream` — retry, do not treat as fatal.
- **Opencode**: free models (`ling-3.1-flash-free`, `big-pickle`, `mimo-v2.6-flash-free`, …) work through the `opencode` adapter — verified live with a `PONG` completion. Paid models still return insufficient funds. Frequent transient 429 capacity errors; retry.
- **AgentRouter** answers `/models` with WAF HTML; **tokenharbor** balance is zero; **cavoti** has no eligible supplier. Errors surface as 502 with the upstream message — that is expected, not a bug in the engine.
- Only `GET {endpoint}/models` (OpenAI style) is implemented for model discovery.

## Layout
- Provider-specific logic goes in `provider/<name>/`, gateway logic in `io/gateway/`, model-calling helpers in `tools/`. Empty `.gitkeep` dirs (`provider/anthropic`, `provider/gemini`, `io/`) are still reserved boundaries; `provider/opencode/` is now real code.
- `session/` is real Go now (D1 store + context trimming) — the old pseudocode sketches are gone.

## Runtime requirements
- Needs `CF_TOKEN`, `cloudflared`, and outbound HTTPS to `api.cloudflare.com` plus the provider endpoints. No offline mode.

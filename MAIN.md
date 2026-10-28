GOAL:
[COMPLETED] Make the keyless Opencode provider answer by matching the official
            client's request contract — no opencode CLI invocation, no key
[COMPLETED] Deployed and verified on this host: the released build (commit
            796ca63) answers "ENGINE OK" and streaming "STREAM OK" through
            /v1/chat/completions on Opencode/big-pickle with no key
[COMPLETED] Fix the catalog regression the endpoint move exposed: Zen answers
            404 at …/openai/v1/models while chat lives there, so the provider
            now carries a models_url (…/inference/v1) for the probe
[COMPLETED] Capture the CLI's real ClientHello (JA3) and the engine's current
            rustls ClientHello, and diff them
[COMPLETED] Implement the matching TLS stack in aixodia-provider
[COMPLETED] Prove a keyless Opencode round trip (adapter answers "PROBE OK" from
            big-pickle with the app's own system prompt in place)
[COMPLETED] fmt/clippy/tests green (fmt 0, clippy 0, 168 tests), including new
            coverage for the anonymous token, env block, chat-first order, and
            null delta fields
[COMPLETED] Commit, push, watch CI + Release, deploy, and verify a real engine
            turn on the Opencode provider (both non-stream and stream)
[COMPLETED] Close the open question from the last session: the official opencode
            client answers big-pickle on this host — twice in a row, rc=0 each
[COMPLETED] Authenticate the GitHub CLI and clone Tulipskun/aixodia-engine
[COMPLETED] Read AGENTS.md and follow its state-tracking protocol
[COMPLETED] Ship the docs: commit MAIN.md, push, verify the rolling release
[COMPLETED] Complete the open AIxodia goal: write /root/.exit, mark g-65c910b3739d done
[COMPLETED] Fix the bug: the app shows no data loaded from D1
[COMPLETED] Fix the bug: Shift+Tab does not work in the app terminal
[COMPLETED] Commit, push, deploy + rolling release after the fix
[COMPLETED] Fix non-login mode: a built-in free provider that works without the opencode CLI
            (keyless round trip answers; a multi-round tool turn can still hit the
            free tier's serialized 1-req/IP throttle, see NOTE)

PLAN:
[COMPLETED] Byte-diffed fresh ClientHellos: the engine's wreq hello now matches
            the CLI's exactly (JA3 md5 1523504b38f0fae0d881d4b6554aac1b, same
            cipher/extension/groups/sigalgs/ALPN order; only random differs)
[COMPLETED] The gate is not TLS alone: a local MITM that forwarded the CLI's own
            request over Python TLS got the same 429, while the same body over
            our BoringSSL client answered 200
[COMPLETED] Captured the CLI's exact request via a trusted local CA: it posts
            /inference/openai/v1/chat/completions with `Authorization: Bearer
            public`, x-opencode-* headers, `stream_options.include_usage` and a
            system prompt ending in an `<env>` block
[COMPLETED] Replayed captured bodies through our client: chat/completions +
            Bearer public + CLI system prompt (or a bare `<env>` block) answers
            200; tiny/other-prompt bodies get 429 FreeUsageLimit
[COMPLETED] Free tier rate-limits a burst with retry-after ~500s; after that
            even a valid body 429s, so the remaining integration tests must wait
            for the window to reset (NOTE)
[COMPLETED] Wire the contract into the adapter: default key "public", 40-hex
            project id, env-block system message, chat completions first, and
            point the builtin at /inference/openai/v1
[COMPLETED] Wired and proven: adapter prepends the env `<env>` block as its own
            system turn, sends `Authorization: Bearer public` when keyless, a
            40-hex project id, chat completions first, and the builtin now points
            at https://opencode.ai/inference/openai/v1
[COMPLETED] Fixed the parser bug the live stream exposed: the gateway sends
            `content`/`tool_calls` as null on opening and finish chunks, and the
            bare String/Vec made every such chunk fail to decode (empty answer)
[COMPLETED] Tests updated for the measured contract (chat-first, Bearer public,
            env block, null delta fields); temporary probe examples removed
[COMPLETED] Tested whether the env block is accepted as a separate leading
            system message (keeps AIxodia's own prompt): yes
[COMPLETED] Env block as the leading separate system message is accepted with
            the app's own system prompt following it: adapter answered PROBE OK
[COMPLETED] Recon: no tcpdump/tshark on the host; AF_PACKET raw socket works
            as root on eth0 (no HTTP proxy in the environment)
[COMPLETED] Capture the CLI's ClientHello: BoringSSL, no GREASE, 17 ciphers
            (1301-1302-1303-c02b-c02f-c02c-c030-cca9-cca8-c009-c013-c00a-c014-009c-009d-002f-0035),
            ext order 0,23,65281,10,11,35,16,5,13,18,51,45,43(+41 on resume),
            groups 11ec-1d-17-18, ALPN http/1.1 only, JA3 md5 ae5f2d34331c18f27df379f7d4b72e35
[COMPLETED] wreq 0.16.1 chosen: reqwest-compatible surface over BoringSSL
            (btls 0.5.6); TlsOptions confirmed to expose cipher_list,
            curves_list, sigalgs_list, alpn_protocols, grease_enabled,
            extension_permutation, session_ticket, renegotiation, OCSP/SCT and
            min/max TLS version
[COMPLETED] Wired the fingerprint into the engine: openai::Adapter now accepts an
            optional wreq transport (WireResponse abstracts reqwest/wreq), and
            opencode::fingerprint_client builds a BoringSSL client with the
            captured ciphers/curves/sigalgs, ALPN http/1.1 only, no GREASE
[COMPLETED] release.yml installs clang/libclang/cmake for bindgen and passes
            CC_aarch64_unknown_linux_gnu/CXX_* so BoringSSL cross-builds
[COMPLETED] gh auth login --with-token; verify account Tulipskun and scopes
[COMPLETED] git clone https://github.com/Tulipskun/aixodia-engine (branch main)
[COMPLETED] Read AGENTS.md (skills/main1: MAIN.md triggers, reading rules)
[COMPLETED] Write this MAIN.md following the mandated structure
[COMPLETED] Set repo-local git identity, commit MAIN.md to main
[COMPLETED] Push all docs commits; CI + Release engine green
[COMPLETED] Create /root/.exit and complete goal g-65c910b3739d in D1
[COMPLETED] Reproduce: engine /v1/sessions returned [] while D1 held 18 sessions
[COMPLETED] Diagnose: aixodia-db parsed D1's `results` key as `rows` — every
SELECT silently decoded to an empty Vec while writes kept succeeding
[COMPLETED] Fix aixodia-db QueryResult (rename to `results`), add parse test
[COMPLETED] Release 8e57e78, `aixodia update` swapped it in place, engine now
lists all D1 sessions; probe session deleted, 18 remain
[COMPLETED] Locate where keystrokes reach the pty: server is a pass-through,
so suspect the WebView xterm.js client or a bytes path that mangles ESC[Z
[COMPLETED] Fix Shift+Tab (ESC [ Z), build + test, commit and push
[COMPLETED] Reproduce the opencode non-login failure and isolate the cause:
            opencode.ai gates its free tier on the client TLS fingerprint, so no
            header/body/IP/colo change from the engine can unlock it (and
            spoofing the OpenCode client is out of scope)
[COMPLETED] Verify a genuinely keyless OpenAI-compatible upstream end to end
[COMPLETED] Add the "AIxodia Free" built-in and a keyless-provider allowance so
            non-login mode needs neither an API key nor the opencode CLI
[COMPLETED] Accept a chat response that omits `content` (a tool-call turn) so a
            keyless OpenAI-compatible upstream can be parsed by `complete` too
[COMPLETED] cargo fmt --check + clippy -D warnings + 165 tests green; commit
            5d9415d pushed; CI and Release engine both green
[COMPLETED] `aixodia update` swapped the new engine in place (pid 3416), the log
            reports total=2 builtin=2, and a keyless round trip answered
            "KEYLESS OK" from AIxodia Free/openai-fast with no key anywhere
[COMPLETED] Repoint engine/settings main to AIxodia Free/openai-fast and move all
            9 chats pinned to Opencode onto it (0 remain pinned)
[COMPLETED] Fix the bug the live turn exposed: WireFunction required `name`, so
            every streamed argument fragment failed to decode and the tool call
            arrived empty; add a fake-upstream test that folds a streamed call
[COMPLETED] Gate green (fmt 0, clippy 0, 166 tests); commit be6fd2d pushed
[COMPLETED] CI + Release engine green for be6fd2d; `aixodia update` swapped it in
            at 22:27:55Z (engine restarted, listening on 127.0.0.1:8787)
[COMPLETED] Test `opencode run -m opencode/big-pickle` on this host: the official
            client answers (legit relay path is real)
[COMPLETED] Check host state: opencode v2.2.0 at /root/.opencode/bin/opencode;
            engine healthy pid 3416 v1.153 idle=true
[COMPLETED] Run one minimal prompt via the official CLI: `opencode run -m
            opencode/big-pickle` answered "CLI OK", rc=0 — the client itself is
            entitled, so the engine-side gate is client identity (as suspected)
[COMPLETED] Second back-to-back call to measure the free tier's serialization
            from the client's side: also answered "CLI OK 2", rc=0 — no 429
[COMPLETED] Prove one streamed agent turn on the deployed engine: run
            run_92a5d66a71f2acd1 (be6fd2d build) called `read` with
            {"filePath":"/etc/hostname","reason":...} and got back
            "1: 439c28b23d3c" in 673 ms, 0 invalid-arguments; round 4 then hit
            upstream 429 (free tier answers one request at a time per IP), so the
            run ended `failed` after the tool result. Probe session deleted (200),
            its 2 orphaned objectives (g-d4c1df6039a2, p-86701a7883ec) deleted
            from D1; 0 rows left, sessions=19

NOTE:
- Operator directive 2026-10-27 (mid-task): after the fix is done and the gate
  is green, commit, push, deploy and cut the rolling release.
- Capture method: /tmp/sniff_ja3.py binds an AF_PACKET raw socket on eth0, parses
  TLS ClientHellos, filters by SNI, and prints JA3 + extension order + raw hex.
- Goal 2026-10-27: make the keyless Opencode path work by matching the client's
  TLS fingerprint in-engine. The operator explicitly ruled out driving the
  opencode CLI, so the adapter must speak the wire protocol itself.
- 2026-10-27: `opencode run -m opencode/big-pickle` works on this host (v2.2.0
  at /root/.opencode/bin/opencode). Superseded note: the operator later asked
  the engine to present the client's contract itself, so the engine may mirror
  it; it still must not invoke the CLI.
- Opencode keyless 2026-10-28T00:15Z: the gate is not the TLS hello alone. A
  local MITM that replayed the CLI's own request over Python TLS got 429, while
  the same body over the engine's BoringSSL client got 200. Verified contract:
  /inference/openai/v1/chat/completions, `Authorization: Bearer public`, the
  x-opencode-* headers with a 40-hex project id, and a system prompt carrying
  the client's `<env>` block (the gate reads it; a generic prompt is refused).
- Free tier quotas: after a burst of probes the endpoint answers 429 to every
  body with `retry-after` around 500s; valid bodies answer again once that
  window passes, so a failing test within the window proves nothing.
- Push 2026-10-27: this MAIN.md (probe record + non-login mode closure) is a
  docs-only commit on main; the rolling Release engine rebuilds it regardless.
- Probe 2026-10-27T22:33Z: a keyless agent turn takes 4 upstream rounds (goal_add,
  plan_add, read, answer). AIxodia Free serializes one request per IP, so round 4
  three seconds after round 3 got HTTP 429 and the run failed after the tool
  result. Nothing is wrong with the tool-arguments fix (read ran with real args);
  the open item is engine backoff between rounds for keyless providers.
- /tmp/verify_stream.sh's last line counts sessions under a stale `data` key; the
  endpoint answers {"sessions":[...]}, so only that count line errors (cleanup
  still runs). Probe side effects: goal_add/plan_add rows were deleted from the
  D1 `objectives` table, scoped to the probe session id.
- Streamed tool calls are folded by `WireToolCall`/`WireFunction` in openai.rs. The
  opening fragment carries id+name and an empty `arguments`; every later fragment
  is `{"index":n,"function":{"arguments":"..."}}` with no name at all, so a
  required field there silently drops the rest of the call — the loop's
  `let Ok(part) = ... else { return Ok(()) }` skips an undecodable chunk.
- Non-login mode upstream, measured 2026-10-27: text.pollinations.ai/openai is
  genuinely keyless but its anonymous tier allows 1 reply per 30 s per IP and
  answers 402 FREE_LIMIT in between; it publishes exactly one model id
  (openai-fast, served by gpt-oss-20b).
- Non-login mode now has two built-ins, in this order: "AIxodia Free" (openai
  adapter, keyless) then "Opencode" (opencode adapter). The engine no longer
  refuses a provider for having no keys: a keyless provider is asked
  unauthenticated and the upstream's own 401 is what the phone shows.
- Opencode's /models is open to the engine (it lists 20+ models); only its
  inference endpoint is client-gated, so that provider probes reachable and still
  cannot answer — which is why the app says so instead of implying it works.
- A keyless turn costs 1636 prompt tokens on the engine's own system prompt and
  tool list, so the free tier's per-reply throttle, not context, is the limit.
- The engine's bearer token is CF_TOKEN and /api/* is guarded by it: curl -H
  "Authorization: Bearer $CF_TOKEN" http://127.0.0.1:8787. Use it from the
  environment and never print it.
- A chat's model is addressed as "Provider/model" (split on the first '/'), so
  the keyless built-in is "AIxodia Free/openai-fast".
- AGENTS.md rule: every non-reading tool call must be accompanied by a MAIN.md update.
- Reading rules: explore with find; read files in full; narrow searches by exact name.
- Token hygiene: the PAT lives only in the gh keyring; never commit or echo it.
- Release is rolling: push to main rebuilds linux amd64/arm64 and clobbers engine-latest.
- Selfupdate exec-swaps the binary in place (/usr/local/bin), keeping pid.
- The app is Tulipskun/AIxodia (Android Kotlin native TerminalView, no xterm.js):
  its half of this fix is the built-in wording and the error mapping, commit
  3c34719.
- Shipped 2026-10-28: 796ca63 pushed (git push needed the repo-local helper
  `!gh auth git-credential`; HTTPS had no stored credential for the remote).
  CI green in 5m18s; Release engine green in 7m33s, including the BoringSSL
  aarch64 cross-build. Rolling release now serves the fixed build.
- Deploy 2026-10-28: `aixodia update` timed out because the running engine
  (started 19:46 under a parent that blocked SIGUSR1) had SIGUSR1 masked in
  every thread, so the signal sat pending and handle_signal never ran. The
  documented fallback worked: `aixodia-engine update` installed the published
  build, then stop/start restarted into it. The fresh process has a zero mask,
  so the next in-place update handshake works normally. Not a code bug.
- Verification 2026-10-28 00:45Z, deployed engine pid 136612 (build published
  00:34:36Z): Opencode/big-pickle answered keyless over /v1/chat/completions —
  "ENGINE OK" (finish stop, 1688 prompt / 27 completion / 1715 total) and
  "STREAM OK" as SSE deltas; builtin endpoint now
  https://opencode.ai/inference/openai/v1.
- In-place update mechanics: SIGUSR1 -> notify_update -> execve keeps the pid and
  arguments; exec preserves a blocked signal mask (and ignored dispositions),
  which is why the old process could never receive the signal after inheriting
  that mask.
- Catalog regression 2026-10-28: after the chat base moved to
  https://opencode.ai/inference/openai/v1, the provider refresh went 404 and the
  provider read as unreachable with no models. Measured: …/openai/v1/models and
  …/inference/models are 404; …/inference/v1/models and …/zen/v1/models answer
  200 with the same list (big-pickle first). Fix: `Provider.models_url` (empty
  means "use api_url"), set on the builtin, used by fetch_models; the probe now
  reflects the real catalog and /api/models carries the Zen ids again.
- Why not the whole api_url at …/inference/v1: both …/inference/v1 and
  …/inference/openai/v1 expose /chat/completions (both answer 429 to a plain
  curl), but the captured CLI contract posts to …/openai/v1, so chat stays
  there and only the catalog is overridden. models_url is a general base
  override, not an adapter special case.

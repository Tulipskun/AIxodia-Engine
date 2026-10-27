GOAL:
[COMPLETED] Authenticate the GitHub CLI and clone Tulipskun/aixodia-engine
[COMPLETED] Read AGENTS.md and follow its state-tracking protocol
[COMPLETED] Ship the docs: commit MAIN.md, push, verify the rolling release
[COMPLETED] Complete the open AIxodia goal: write /root/.exit, mark g-65c910b3739d done
[COMPLETED] Fix the bug: the app shows no data loaded from D1
[COMPLETED] Fix the bug: Shift+Tab does not work in the app terminal
[COMPLETED] Commit, push, deploy + rolling release after the fix
[IN_PROCESS] Fix non-login mode: a built-in free provider that works without the opencode CLI

PLAN:
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
[IN_PROCESS] Gate green (fmt 0, clippy 0, 166 tests), commit, push, wait for
            engine-latest, `aixodia update`
[IN_PROCESS] Prove one streamed agent turn on the deployed engine: a run whose
            tool call arrives with real arguments, then delete the probe session

NOTE:
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

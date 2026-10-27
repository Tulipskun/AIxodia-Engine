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
[IN_PROCESS] cargo fmt --check + clippy -D warnings + test --workspace, commit,
            push, wait for engine-latest


[IN_PROCESS] Deploy with `aixodia update`, repoint engine/settings at the
            keyless built-in and probe a real chat turn

- Non-login mode now has two built-ins, in this order: "AIxodia Free" (openai
  adapter, keyless) then "Opencode" (opencode adapter). The engine no longer
  refuses a provider for having no keys: a keyless provider is simply asked
  unauthenticated and the upstream's own 401 is what the phone shows.

- The engine's own bearer token is CF_TOKEN, and /api/* is guarded by it, so a
  local probe is: curl -H "Authorization: Bearer $CF_TOKEN" http://127.0.0.1:8787

NOTE:
- Non-login mode upstream, measured 2026-10-27: text.pollinations.ai/openai is
  genuinely keyless but its anonymous tier allows 1 reply per 30 s per IP and
  answers 402 FREE_LIMIT in between; it publishes exactly one model id
  (openai-fast, served by gpt-oss-20b).
- AGENTS.md rule: every non-reading tool call must be accompanied by a MAIN.md update.
- Reading rules: explore with find; read files in full; narrow searches by exact name.
- Token hygiene: the PAT lives only in the gh keyring; never commit or echo it.
- Release is rolling: push to main rebuilds linux amd64/arm64 and clobbers engine-latest.
- Selfupdate exec-swaps the binary in place (/usr/local/bin), keeping pid.
- The app is Tulipskun/AIxodia (Android Kotlin native TerminalView, no xterm.js):
  fix = Shift latch chip + Tab chip sends ESC[Z; app commits cdddcbf/40f7879,
  app release `latest` re-cut 2026-10-27 (versionName 1.0.202610272010).
  The terminal server here is a byte-for-byte pty pass-through, so engine code
  is unchanged — nothing to release in this repo.

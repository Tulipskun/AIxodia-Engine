GOAL:
[COMPLETED] Authenticate the GitHub CLI and clone Tulipskun/aixodia-engine
[COMPLETED] Read AGENTS.md and follow its state-tracking protocol
[COMPLETED] Ship the docs: commit MAIN.md, push, verify the rolling release
[COMPLETED] Complete the open AIxodia goal: write /root/.exit, mark g-65c910b3739d done
[COMPLETED] Fix the bug: the app shows no data loaded from D1
[COMPLETED] Fix the bug: Shift+Tab does not work in the app terminal
[COMPLETED] Commit, push, deploy + rolling release after the fix

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

NOTE:
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

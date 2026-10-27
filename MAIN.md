GOAL:
[COMPLETED] Authenticate the GitHub CLI and clone Tulipskun/aixodia-engine
[COMPLETED] Read AGENTS.md and follow its state-tracking protocol
[IN_PROCESS] Ship the docs: commit MAIN.md, push, verify the rolling release
[IN_PROCESS] Complete the newest open AIxodia goal: write /root/.exit (restart
handshake) and mark g-65c910b3739d done in the D1 objectives store

PLAN:
[COMPLETED] gh auth login --with-token; verify account Tulipskun and scopes
[COMPLETED] git clone https://github.com/Tulipskun/aixodia-engine (branch main)
[COMPLETED] Read AGENTS.md (skills/main1: MAIN.md triggers, reading rules)
[COMPLETED] Write this MAIN.md following the mandated structure
[COMPLETED] Set repo-local git identity, commit MAIN.md to main
[IN_PROCESS] git push main; confirm release.yml rebuilds engine-latest green
[IN_PROCESS] Create /root/.exit and complete goal g-65c910b3739d in D1

NOTE:
- AGENTS.md rule: every non-reading tool call must be accompanied by a MAIN.md update.
- Reading rules: explore with find; read files in full; narrow searches by exact name.
- Token hygiene: the PAT lives only in the gh keyring; never commit or echo it.
- Release is rolling: push to main rebuilds linux amd64/arm64 and clobbers engine-latest.

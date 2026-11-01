GOAL:
[IN_PROCESS] A backup function that stores data in the HF storage bucket the
            operator named (operator: "สร้างฟังชั่น backup ข้อมูลโดยเก็บไว้ใน HF
            <hub write token, not repeated here> และหาวิธีที่เร็วที่สุด ทั้งการ
            backup และ restore"). Scope settled by the operator: the tree is
            `/root` minus caches and image-supplied directories, the trigger is
            the phone's `/` menu, the hub token lives in D1 state, and a restore
            has to be confirmed explicitly.
            Facts measured on this host before choosing a mechanism:
            (a) the bucket is `kyomu53n/Bucket`, private, listed by
            `GET /api/buckets/{namespace}`; (b) the S3 gateway `s3.hf.co`
            refuses this token (`InvalidAccessKeyId` for SigV4, `invalid header:
            authorization` for bearer) because it needs separately generated
            `HFAK` credentials — so the gateway is out and every transfer goes
            through the hub API; (c) upload needs Xet (`POST
            /api/buckets/{id}/batch` takes an `xetHash` that must already exist
            in the CAS), and the Rust crate `hf-xet` exposes exactly the session
            API the python client uses — proven end to end from Rust: file →
            `XetUploadCommit` → hash `c409418…` → batch 200 → `resolve` GET →
            byte-identical; (d) throughput is 26 MB/s up and 37–64 MB/s down per
            stream, and 8 ranged GETs of one 64 MiB object were *slower* (2.24 s)
            than one GET (1.78 s), so restore parallelism belongs across files,
            not inside one; (e) Xet dedups at chunk level — re-uploading an 8.5 MB
            SQL dump that had grown by 24 KB cost 0.51 s against 1.21 s for the
            first copy, which is why the tree is stored as content-addressed
            objects and never as one compressed tarball.
            Mechanism: `rust/crates/aixodia-backup` — objects keyed by file
            sha256 under `<prefix>/objects/aa/<sha>`, one manifest per run under
            `<prefix>/manifests/<utc>.json`, `<prefix>/latest.json` as the
            pointer. Unchanged files (size + mtime match the previous manifest)
            are stat'ed only and reused, so a steady-state backup costs a couple
            of round trips; changed bytes move through Xet.
[IN_PROCESS] The engine side: `aixodia-io/src/backup.rs` puts the five routes
            on the gateway — `GET/POST /api/backup` (status, start),
            `GET /api/backup/list`, `PUT /api/backup/config` and
            `POST /api/backup/restore`. The config, hub token included, is a
            single JSON value in D1 state under `backup/config`; a write that
            omits a field keeps the stored one, so the app can set the bucket
            without ever resending the token, and nothing but a tail of it is
            ever reported back. One job slot, `409` while it is held, and the
            worker reports through a `Reporter` the status route reads.
            Blocking bucket work never runs on a tokio worker: `reqwest::blocking`
            refuses to be built in a runtime context and `hf-xet` panics on a
            worker, so jobs go to a plain OS thread and `list` is awaited over a
            channel from the blocking pool. A worker that panics releases the
            slot through `catch_unwind`. Restore takes no target path from the
            client — it always rebuilds the configured root — and refuses to run
            at all unless the body says `confirm: true`.
[COMPLETED] The `aixodia-backup` crate itself, compiling and unit-tested (10
            tests). `lib.rs` holds the key layout, `Config`, `Entry`,
            `Manifest`, the progress `Reporter` and `join_under`, which is what
            stops a manifest from naming a target outside the root it is
            restored into. `scan.rs` walks the tree (exclusions, one
            filesystem, symlinks recorded rather than followed) and decides per
            file whether its bytes have to be read at all. `hub.rs` is the
            bucket API: paginated `tree`, `paths-info` existence probes, NDJSON
            `batch` add/delete with per-operation failure reporting, and
            `resolve` downloads into a temporary sibling that is renamed only
            once complete. `xet.rs` ingests through `hf-xet`, one commit per
            1000 objects, on a thread the module owns — the crate's blocking
            calls panic inside a tokio runtime. `ops.rs` runs the two
            operations: a steady-state backup is `latest.json`, its manifest,
            one probe and one manifest write; a restore fans out per file over
            12 threads, verifies every sha256 after writing, and leaves files
            the manifest does not name untouched. A file that disappears
            between the scan and the hash is reported and skipped, not fatal.
            Clippy's five findings on the first pass are taken: the redundant
            reference at the restore spawn, the collapsible size-then-hash test
            in `restore_file`, `as_ref().map(clone)` in `object_keys`, the
            `let`-then-return in `Reporter::new`, and iterating a map's
            key-value pairs to read only the keys in the live example.
            Gate green on the whole workspace: `cargo fmt --all --check`,
            `cargo clippy --all-targets -- -D warnings` and
            `cargo test --workspace` (exit 0) all pass on this host.
            Measured against the real bucket on the real tree (`/root`, prefix
            `aixodia/root`): a first backup of 392 files / 11,028,084 bytes took
            **4,289 ms** and uploaded 10,999,649 of them; a later backup of the
            same tree took **2,906 ms**, uploaded 218,328 bytes (the files that
            genuinely changed) and pruned 1 manifest + 4 objects no retained
            manifest needed; a backup of a tree with nothing changed at all took
            **1,897 ms** and uploaded **0 bytes** — every file reused from the
            previous manifest, which is the steady-state claim on the wire.
            A restore into an empty directory pulled all 392 files, 216 dirs and
            1 symlink back in **6,442 ms** over 12 threads and every entry
            matched (`MATCH: 609 entries identical`); restoring the same backup
            again took **598 ms** with 392 files recognised as already correct.
            Five entries were reported and skipped rather than fatal: jupyter
            kernel sockets, which are not regular files.
            `examples/live.rs` had a bug in its own check, found by running it:
            `fingerprint` walked the source tree with *no* exclusions while the
            backup had walked it with the defaults, so the comparison would have
            hashed 20 GB of `target` and caches and then reported a tree full of
            phantom differences. It now fingerprints with
            `scan::DEFAULT_EXCLUDES`, the same list the operation used.
            The second run then failed on exactly one entry, and the entry was
            honest: `.aixodia/aixodia.log`, the live engine's own log, which grew
            between the manifest and the comparison. So the check no longer
            demands that a running machine be still — the source's mtime is
            compared with the moment the manifest was read, and a file written
            after that is reported as `CHURN` instead of a mismatch, while a
            restored file that does not match what was recorded, or an entry that
            was not written at all, still fails the run.
[COMPLETED] Add the crate to the workspace (`aixodia-backup`, `hf-xet 1.8`,
            `xet-data 1.8`, `http 1` as workspace deps).
            `reqwest 0.12` stays for the rest of the engine and `hf-xet` pulls
            its own `reqwest 0.13`; two versions coexist rather than churning
            every provider call in the workspace.
            `gateway.rs` VERSION is bumped to `v1.164` for this: the routes are
            new surface, so the build the phone sees has to name itself.
[COMPLETED] An in-place update must not leave the previous build's cloudflared
            running (operator: "เก็บกวาดตัวเก่า แล้วจัดการป้องกันไม่ให้เกิดขึ้นอีก"). The
            orphan 5910 (`fold-decrease-owners-venture`, up since 00:14:43) was
            SIGTERMed. The prevention shipped in v1.163 (`a83a6f1`): a keeper
            records its cloudflared pid in `$AIXODIA_HOME/cloudflared.pid`, and
            the next process image reaps that pid before dialing — verified
            against `/proc/<pid>/cmdline` so a recycled pid is never signalled,
            and `waitpid`ed so the dead tunnel does not linger as a zombie under
            the pid `execve` preserved.
[COMPLETED] 429 classification must read the upstream's own wait, not guess at
            its wording (operator: "ถ้ามีบัคก็แก้บัคก่อน", after asking how to change
            IP to dodge the Opencode 429). The IP route was ruled out first: this
            egress is a shared GCP address (34.75.4.47), so rotating lands on
            other equally-pooled addresses, and multiplying an anonymous free
            allowance is circumventing a limit the provider set on purpose. Then
            the premise was tested live and is false today: two keyless calls two
            seconds apart both answered 200 (`created` 1793492639 ->
            1793492641), so the `1-req/IP` serialization that killed round 4 on
            Oct 27-28 is not active upstream, and adding inter-round pacing would
            slow every turn to defend a condition measured absent. IPv6 was ruled
            out the same way on 2026-11-01, on the record: this host has no global
            v6 address and no v6 default route (`ip -6 addr show scope global` and
            `ip -6 route show default` both empty; `ip -6 addr` is 3 lines — ::1 on
            lo), while opencode.ai is v6-reachable (AAAA `2606:4700:78::90:0:*`,
            Cloudflare; `curl -4` lands on 172.65.90.20). So the blocker is entirely
            our side, and clearing it would only be another way to present as a
            different client — which is the thing declined above. It is also not
            clearly a fresh allowance: at Cloudflare scale a limit is commonly keyed
            on the prefix rather than the single address. What IS a
            defect, provable by reading: `APIError` carried only
            `{status_code, message}`, `error_message` returned only
            `error.message` (dropping `error.type` and every timing field), and
            no file in the workspace read `retry-after` at all. So
            `quota_is_terminal` decided a hard quota from 8 substrings. Both free
            tiers phrase the momentary window and the spent allowance with the
            same wording (measured Oct 28: all eight free-marked Opencode models
            answer `429 FreeUsageLimitError`), so the guess is wrong in both
            directions — a rideable throttle whose text says "free usage limit"
            dies on attempt 1 (the regression `040cf55` introduced while fixing
            the 31s stare), and a spent allowance whose number sits in
            `retryAfterMs` while `message` is empty is retried for the full
            3/8/20s. Fix: carry the wait, then let it decide.
[COMPLETED] v1.162 shipped the fix. `APIError.retry_after: Option<Duration>` with
            `with_retry_after`; `error_wait` prefers the header and falls back to
            the body; `retryable` judges a 429 by that number against
            `RIDEABLE_WAIT` (31s, pinned to `KEYLESS_BACKOFF` by a test so neither
            can drift) and reaches for the wording only when no number arrived;
            the keyless retry now sleeps the window the upstream named instead of
            a blind 3/8/20. Carried at every boundary the fingerprint client had
            blinded: `WireResponse` in both the reqwest and the wreq arm,
            `Sent::Buffered`, five openai sites, two anthropic sites, and the
            `APIError` the Opencode ladder rebuilds. Gate: build 0,
            `cargo test --workspace` 191 passed / 0 failed, clippy `-D warnings`
            0, fmt 0, launcher and installer suites 0 — run locally because this
            host had no Rust toolchain at all (installed stable 1.99.1 with
            rustfmt + clippy, plus clang/libclang-dev/ninja for BoringSSL). Four
            new tests, all verified by name. What it does not buy: a throttle
            that states no number still falls through to the wording heuristic and
            behaves exactly as before — inventing a wait would be the same guess
            this change removes, and the live `1-req/IP` throttle could not be
            reproduced today to learn its shape.
[COMPLETED] Deployed v1.162 and proved the fail-fast path on the wire. CI
            41260352899 and Release engine 41260353067 both green on 4ad9cea;
            asset 17,830,048 bytes digest sha256:92eb21bd…, distinguishable from
            the v1.161 image (17,821,856) by size as well as digest. `aixodia
            update` swapped it in place on pid 5832 — the listener went dark
            ~12s (01:23:03 engine started → 01:23:11 listening) while the new
            image re-verified D1 and re-registered the tunnel, `daemon url saved`
            01:23:17, `handover: note written`, and `/healthz` then answers
            `{"pid":5832,"started":1793496182,"version":"v1.162"}` with etime
            still counting from the original start, which is the exec signature.
            A live Opencode turn came back HTTP 502 in **98 ms** with a single
            attempt in the trace, so the wording fallback both refuses a spent
            allowance and refuses to stall the phone — the `040cf55` contract
            intact on the new classification code. Pollinations answers normally
            on the same build ("POLL162 OK", 1823 prompt tokens). 0 WARN/ERROR.
[COMPLETED] D1 must be a backup, not a step of the turn (operator: "ไม่ใช่เพราะ
            error 429 แต่ในสถานการณ์ปกติมันก็ช้าเหมือนกัน" then "D1 ทำหลังจากกระบวนการทุกอย่าง
            ไว้เป็น backup ไม่ต้องรอ D1 เขียนเสร็จ"). Measured on localhost with a
            warm provider: the first `request` block only appeared at +4.65s,
            +4.86s, +7.15s across three turns, and the provider answer itself
            took ~1s. The waiting is `aixodia-db` — every call is an HTTPS
            request to the Cloudflare REST API (~0.6–1.2s each) — and a turn
            made them in series before it could ask the model anything:
            `sessions.get`, `sessions.turns`, the user-row write
            (`persist_user_before`), the run row write before the 202
            (`store.insert`), then a `trail.flush` write between every tool
            round, and the answer write before the `final` frame. So: the two
            session reads run beside each other, the run row is written behind
            the 202, and every turn write goes onto a per-chat backup queue
            that the turn never waits on, serialized so the question lands
            before the reply and each round's tool steps before the next
            round's. The next turn drains that chat's queue before reading
            history, so the model still sees its own last answer; a chat whose
            writes already landed pays nothing. The `final` frame keeps a turn
            object built from the response in memory (seq 0) and says
            `turns_synced: false` — the phone may not trust that seq, and what
            it does about it is written below. Caught while proving it: a queued write that holds a
            strong sink holds the SSE sender with it, so the turn finished and
            `[DONE]` was written while the response body stayed open — the read
            never returned. A row write now reports through a `Reporter`, either
            `Live` (the turn waits, so the strong sink costs nothing) or
            `Behind` (weak, upgraded only at the moment it reports), and the new
            test a_store_that_takes_its_time_does_not_hold_the_answer parks the
            store mid-write and asserts both the closed stream and the rows
            arriving in order once it is released. What it costs: a turn's rows
            are this process's to finish, so an exit drains every chat's queue
            (five seconds, then it gives up) rather than taking an owed write
            down with it, and an in-place update — which replaces the process
            with `execve` and never reaches that exit — now waits for the same
            counter to reach zero before it installs. A kill inside that window
            can still lose the last turn's rows, which the phone shows from its
            own copy. What the phone did need: an answer that arrives with seq 0
            is not a row its `onFrame` used to write, so the live copy of the
            reply was dropped at the closing frame and the bubble left the thread
            until a pull brought it back. The app now commits an unnumbered
            answer at the next free seq of its own, and reads the store again
            2.5s after the turn — long enough for D1 to have kept the write — so
            the engine's numbered row replaces the phone's copy on the same
            screen (AIxodia, same day). Checked on the wire, not assumed: a run
            through the poll endpoint showed the `final` event persisted as
            `{turn, usage}` — sink.rs drops `turns_synced` in that projection —
            with the turn inside it at `seq: 0`, D1 holding only the question at
            the moment the stream closed and both rows 2.5s later. The seq is
            therefore the word the phone can always rely on, and the flag is
            read when a transport carries it. Shipped: c646b36 plus 0d2d4c3 — CI
            41194131602 and Release engine 41194131609 both green — and `aixodia
            update` swapped pid 1649 in place at 10:59:38. Measured through the
            live tunnel afterwards: the first `request` block at +0.62s and
            +0.93s (was +4.65/+4.86/+7.15s), `[DONE]` and the closed body at the
            same instant, the rows landing in seq order after the answer, and a
            second turn answering "SYNCOK" out of the first turn's stored
            history — the drain before reading history is what makes that work.
[COMPLETED] A spent free-tier quota must fail fast, not stall the phone for
            half a minute (operator: "แก้ไขความเร็วหน่อย มันไม่ควรช้าขนาดนี้",
            screenshot of a first turn stuck on "Sending request to provider…
            17s"). The engine log shows the turn ended in
            "upstream HTTP 429: Free usage limit reached" (Opencode/beam-free).
            Root cause: a keyless free provider retries a 429 up to
            KEYLESS_ATTEMPTS=4 times with the [3s, 8s, 20s] backoff — ~31s of
            sleeping — which is right for a per-IP throttle that clears in
            seconds (Pollinations) but pointless for a hard quota that a wait
            cannot refill, so the user just watches a spinner before the same
            answer. `retryable()` now treats a 429 whose message says the
            allowance is spent ("free usage limit", "limit reached", "subscribe",
            "upgrade", "quota", "insufficient", "out of funds", "billing") as
            terminal and returns false, so the turn surfaces after one call; a
            plain "rate limit"/"too many requests" 429 still paces. New tests
            a_momentary_throttle_is_worth_retrying and
            a_spent_free_quota_fails_fast_instead_of_stalling. Shipped: 040cf55
            — Release engine 41187238156 green, and `aixodia update` swapped pid
            1649 in place at 09:15:18 installing the build published 09:13:36.
            CI 41187238106 was NOT green, as this entry first claimed: the old
            a_keyless_turn_retries_after_a_quota_answer still expected a retried
            429, and the wording it faked ("free tier quota") became terminal
            with this very fix, so its assert panicked, poisoned the shared
            config lock and failed ~25 gateway tests behind it. That was a stale
            test, not a lost contract — the retry-a-throttle case is now mocked
            with "too many requests" and still passes (c646b36; its CI
            41193504711 is green, and the Release that commit queued,
            41193504692, was superseded — the shipped bytes come from 0d2d4c3's
            run).
            It was the fix for the log-visible failure, not for the ordinary
            slowness; see the entry above.
[COMPLETED] A tool block must carry its own done-ness, not infer it from an
            empty result (operator: "การแสดงผลบัคอีกแล้ว" — a `rm` shell card spun
            forever). `tool_block` derived STATE_DONE only when the result text
            was non-empty, so a successful command that printed nothing
            (`rm`, `mkdir`, `cd`) came out "running". `tool_block` now takes an
            explicit `has_result` and a quiet success is STATE_DONE; the stage
            at each of the six call sites (chat.rs, trail.rs, mobile lib.rs) is
            what sets it. New test a_quiet_success_is_done_not_running; the
            existing settle test updated for the new parameter. fmt clean,
            aixodia-session 16 passed (aixodia-io/mobile included), clippy
            exit 0. Shipped: 98abef7 — CI 41183703843 and Release engine
            41183703811 both green; `aixodia update` swapped pid 1649 in place
            (self-update "installing the build published=2026-10-31T08:19:42Z",
            this commit's CI, engine started 08:23:09), so the running daemon
            now emits state=done for a quiet success. Live prove of the
            empty-result path was blocked upstream, not by this change: a
            throwaway session's turn died on "upstream HTTP 429: Free usage
            limit reached" (free-tier quota) before any tool_result was made —
            that probe session is deleted. The wire schema was still confirmed:
            tool events carry `data.stage` (tool_call/tool_result/usage) and
            `data.block.state`, exactly the field the app now trusts.
[COMPLETED] A bare `aixodia` starts nothing (operator: "ไม่ ถ้าใช่แค่ aixodia จะไม่ทำงาน
            แต่จะแสดง version และวิธีการใช้งานแทน" then "aixodia status only"). It
            printed the version the running engine reports through /healthz — the
            only version the launcher can honestly know, since the binary takes no
            `--version` — or that there is no engine to report, then the usage.
            Starting is `aixodia start`, which keeps the log follow from the entry
            below; `help`/`-h`/`--help` answer the same as the bare command and an
            unknown word names itself, prints the usage and exits 2. Launcher tests
            grew to cover all of it, and the handshake stub's port is now read
            through `wait_for_port` because the stub prints its url before it binds
            — the three single reads raced and the suite failed one run in three.
            Three clean runs here, and the installed launcher on this host is this
            build.
[COMPLETED] A start from a terminal should not hand the prompt back (operator:
            "ต่อไป ปรับให้ คำสั่ง aixodia เป็น aixodia start และไม่ต้องคืน shell
            แต่ให้เป็นการ print log ออกมาแทน") — read with the entry above: the
            log follow belongs to `aixodia start`, and the bare command that this
            entry left starting the engine is now the version-and-usage answer. Bare `aixodia` already meant
            `start`; what it did after starting was report the url and exit. It now
            follows the log — `tail -n +1 -f` on the fresh file — so the terminal
            shows the engine instead of a prompt, and Ctrl-C stops the reading, not
            the engine (it is nohup'd and detached). The follow is gated on a real
            terminal, so scripts and CI that run `aixodia start` still get their
            prompt back and can reach `stop`; `update`'s restart passes
            `nofollow` for the same reason. Two launcher tests added (a `script`
            pty plus a `timeout` for the Ctrl-C); all 23 launcher cases pass here,
            and the installed launcher on this host is the new one — verified
            against the running engine, which stayed up after the follow was cut.
[COMPLETED] The phone's last grid row was still a tmux status bar (operator's
            screenshot: `[aix-t-e61<der>"◆ …" 07:46 30-Oct-26`). `tmux::start` did
            ask for `status off`, but as a *second* client run after the spawn —
            the server may not exist yet and the new name may not be registered
            yet, and the failure was swallowed: stderr to null, result ignored, and
            a `sleep(Duration::from_millis(0))` where a wait used to be. Both live
            sessions carried no session-level value at all, which is what the bar
            on screen proved. The option is now chained into the creation command
            (`new-session … ; set-option -t <name> status off`) so the server
            applies it in the same round-trip it registers the session, and boot
            calls `tmux::hide_status_bars()` to quiet the sessions a restart only
            adopts. Applied to the running server by hand and verified: both
            `aix-t-…` sessions report `status off`. Shipped as ffc2094: CI green
            on build, test, `cargo fmt --check` and clippy `-D warnings`, and the
            Release engine run published alongside it.
[COMPLETED] Surface goals, plans, and compaction to the user (v1.160):
            (1) objectives keep a status (open/done/dropped) in D1 and
            MemoryStore, `Store::all()` reads every status; (2) every
            goal_*/plan_* mutation mirrors the whole list to
            `<set_directory>/aixodia-goals.md` in Obsidian format (frontmatter,
            task checkboxes, `#done`/`#dropped` sections; set_directory writes
            it once too); (3) GET /v1/sessions/:id/objectives answers
            {goals:[{id,content,status}], plans:[...]} for the app's card;
            (4) `compact()` reports what it dropped — a new ChatSink
            `on_compact` emits `tool_event stage:"compact"` with kept_turns,
            dropped_turns and a per-turn digest, which the run sink forwards
            verbatim. fmt/clippy 0, workspace tests green.
[COMPLETED] Add a set_directory(path) tool so the shell runs where the chat
            points it instead of a `cd` prefix on every command. The choice is
            kept per session in aixodia-tools::dir (a global map keyed by
            session_id), and run_shell prefers an explicit workdir, then that
            directory, then HOME. An empty path clears it; a relative path
            resolves against the directory already set; canonicalize rejects a
            missing path or a plain file. Listed in builtin/builtin_list and
            reachable through tools_with. fmt/clippy clean, aixodia-tools 17
            passed, cargo check --workspace --all-targets green.
[COMPLETED] Show the program's window title (OSC 0/2) in the terminal drawer
            header instead of nowhere. tmux swallows OSC 0/2, so the engine now
            reports #{pane_title} in /v1/terminals rows (tmux::list +
            Info.title) and the app carries it to TerminalSidebar. Operator's
            original symptom "<host>:bash …" was tmux.conf's status-left,
            superseded because tmux.rs disables the status bar per session.
            VERSION v1.159; fmt/clippy clean, aixodia-io 70 passed,
            aixodia-terminal 4 passed.
[COMPLETED] Make keyless Opencode the daemon's default route: default_settings
            names Opencode/big-pickle and saved_main_model falls back to it when
            nothing usable is stored, so a fresh/unset daemon answers on the
            built-in non-login provider. No auto-fallback to Pollinations (the
            operator chose strict). Shipped v1.157 (bce6c62) and applied live:
            in-place `aixodia update` moved pid 1136 v1.156→v1.157, and
            PUT /api/settings set main=Opencode/big-pickle (GET confirms).
[COMPLETED] Diagnose why the operator cannot use the app end to end and fix it:
            (1) a keyless provider's catalog now parses the bare-array shape and
            reports reachable; (2) keyless 429/402 retries 3/8/20s and no longer
            opens the breaker; (3) a new route-less chat answers from the saved
            main; (4) self-update refuses a stale CDN payload by published size
            and sha256 digest instead of recording it as applied. Shipped
            719b047, ddb8730, 607930d, 808a504; v1.156 is deployed (pid 176154)
            and verified live on /v1/runs plus /v1/chat/completions with
            AIxodia Free/openai-fast.
[COMPLETED] Make the keyless Opencode provider answer by matching the official
            client's request contract — no opencode CLI invocation, no key
[COMPLETED] Deployed and verified on this host: the released build (commit
            796ca63) answers "ENGINE OK" and streaming "STREAM OK" through
            /v1/chat/completions on Opencode/big-pickle with no key
[COMPLETED] Fix the catalog regression the endpoint move exposed: Zen answers
            404 at …/openai/v1/models while chat lives there, so the provider
            now carries a models_url (…/inference/v1) for the probe
[COMPLETED] Ship it: 941000a, CI + Release green, in-place update kept pid,
            refresh now reports 84 models (big-pickle first), and a smoke turn
            answered "DEPLOY OK" on the new build
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
[COMPLETED] Fix the terminal's pid across an engine stop/restart: a tmux-backed
            session reported the pid of the tmux *client* its pty runs, which is
            replaced on every attach, so the phone's tab changed pid after a
            restart even though the shell behind it never did. The engine now
            reports the pane's pid (tmux::pane_pid) as the session's pid.
            Shipped 3a9891a; `display-message -p '#{pane_pid}'` returned 1007 for
            the session `list-sessions` names, and the deployed build is live
            (see the deploy line).
[COMPLETED] Fix a closed terminal tab coming back: DELETE /v1/terminals/:id only
            looked in the in-memory map, and a restarted engine has an empty map,
            so it answered 204 while the tmux server kept the shell alive — this
            host still holds three such shells (aix-t-5b864fae…, aix-t-c14dbc89…,
            aix-t-eb544588…). The close now kills the tmux session too. They are
            still attached: killing them is the operator's call (or a hold on the
            tab in the app once the new APK is installed).
[COMPLETED] Deploy it: CI 40652801149 and Release engine 40652801145 green,
            engine-latest amd64 17,731,584 bytes sha256 2c74b0da…98ff, and the
            daemon installed it itself — aixodia.log "selfupdate: installing the
            build published=2026-10-28T07:58:10Z", /healthz then answers
            {"ok":true,"pid":528,"started":1793174297 (=07:58:17Z),"version":
            "v1.158"}. Note for next time: `aixodia update` compares the FILE on
            disk with the published one, so "already the published build" says
            nothing about the running image — prove it with the healthz
            `started`/selfupdate log, since ps keeps the original start time
            across the re-exec and /proc/<pid>/exe follows the path, not the
            inode.
[COMPLETED] Fix "Upstream request failed: [400] Assistant tool call
            function.arguments must be valid JSON" on the Opencode ladder. The
            engine replays an assistant turn's tool calls verbatim; a call whose
            arguments never parsed (stream cut mid-JSON, or a model that named
            the function and filled nothing) makes every later round of the turn
            a 400, and `is_tool_protocol_error` does not match "must be valid
            JSON", so the ladder aborts on attempt 1. The OpenAI chat and
            Responses wire paths now substitute `{}` for empty/unparseable
            arguments (the Anthropic path already did), leaving the stored call
            untouched. Regression test captures the outgoing body. Shipped
            4c9e2a0 (VERSION v1.161); CI 41150390919 and Release engine
            41150390939 green, engine-latest amd64 17,715,296 bytes sha256
            ca12978f…12fb. Deployed in place, pid 1649 kept: the first `aixodia
            update` read a stale unauthenticated release listing and answered
            "already the published build"; the retry 74 s later logged
            "installing the build published=2026-10-31T00:36:57Z", restarted at
            00:38:41 and /healthz now reports v1.161 (started 1793407120). A
            live turn on Opencode/big-pickle answered "DEPLOY OK".

PLAN:
[COMPLETED] (1) record the spawned cloudflared pid — `Tunnel.pid` from
            `child.id()`, written to `$AIXODIA_HOME/cloudflared.pid` by the
            keeper on every start; (2) `reap_stale_tunnel` at the top of
            `Keeper::run`: read the pid, confirm `/proc/<pid>/cmdline` really is a
            cloudflared `tunnel` before signalling anything (a zombie has an empty
            command line and a recycled pid names another program, so both read
            false), refuse pid <= 1 and our own pid — which `execve` preserves —
            SIGTERM, give it `STALE_REAP_WAIT`, SIGKILL if it is still up, then
            `waitpid(WNOHANG)` so it does not linger as a zombie under the pid the
            handover kept; (3) `libc` added to the engine crate (already a
            workspace dependency); (4) tests: the recorded pid is a live tunnel, a
            stale one is reaped and a second reap finds nothing, an unrelated live
            process named by a stale pidfile is left running; (5) gate, merge,
            deploy. Chosen over fixing only the handover path, because a crash or
            a SIGKILL orphans the tunnel the same way and a pre-exec cancel would
            not cover either.
[COMPLETED] Deployed as v1.163 and measured. CI 41270440586 and Release
            41270440608 green on `a83a6f1`; asset 17,842,400 bytes digest
            sha256:6e0a5443…, distinguishable from v1.162 (17,830,048 /
            sha256:92eb21bd…) by size as well as digest. Gated on this host before
            pushing: build 0, `cargo test --workspace` 202 passed / 0 failed,
            `cargo fmt --check` 0, `cargo clippy --workspace --all-targets -- -D
            warnings` 0, `aixodia_test.sh` 0, `install_test.sh` 0. `aixodia
            update` swapped in place on pid 5832 (`started` moved 1793496182 →
            1793505315 while the pid held), dialed
            `varied-guides-cat-neil.trycloudflare.com`, `daemon url saved`
            03:55:31, and `/root/.aixodia/cloudflared.pid` now holds 89233 — the
            pid of the one live cloudflared, so the recording half works in
            production. The address v1.162 had published now answers 530 (nothing
            behind it), so exactly one tunnel is up and 0 WARN/ERROR since the
            restart.
[COMPLETED] (1) `APIError.retry_after: Option<Duration>` + `with_retry_after`;
            `body_retry_after` (retryAfterMs/retry_after at top level, under
            `error`, or in the first `errors` entry; numeric or quoted) and
            `header_retry_after` (delta-seconds only — an HTTP-date is no signal,
            and no date crate is pulled in for a form these gateways do not
            send); (2) `retryable` decides a 429 by the wait against
            `RIDEABLE_WAIT` when there is one, and by the wording only when
            there is not; (3) carry the value at every wire boundary —
            `WireResponse` in both the reqwest and the wreq arm (the fingerprint
            client dropped headers when it was introduced), `Sent::Buffered`,
            the three openai and two anthropic error sites, and the
            `APIError::new` that rebuilds the error across the Opencode ladder;
            (4) `chat.rs` sleeps the stated wait when it is within budget
            instead of always 3/8/20s; (5) a test pins `RIDEABLE_WAIT` to
            `KEYLESS_BACKOFF` so the budget cannot drift; (6) fmt/clippy/tests,
            VERSION bump, commit, push, watch CI + Release. The operator approved
            gating locally over opening a PR, and merge + in-place update once the
            gate was green.
[COMPLETED] Provider: repair replayed tool-call arguments at the OpenAI chat
            and Responses wire boundary; regression test; fmt/clippy/tests;
            bump VERSION to v1.161; commit, push, watch CI + Release, deploy
            with `aixodia update`, verify a live turn.
[COMPLETED] Fix the blank terminal after a reconnect (operator: "หลังจากการ
            reconnect เห็นหน้าจอว่าง ต้องเปิด/ปิดแป้นพิมพ์ก่อน 1 ครั้งถึงจะเห็น
            หน้าจอ"). Under tmux the attach sends only the RIS reset — the ring
            is dropped by replay_frame — and the pane's screen lives in the tmux
            server, which repaints the client only when it gets a SIGWINCH. The
            old kick fired only for an alternate-screen session on a same-size
            attach, so a plain shell reattached to the pty that still ran the
            first client came back to a cleared grid with nothing behind it;
            opening the keyboard changed the size, TIOCSWINSZ signalled the
            client, and the screen came back — which is exactly the workaround
            the operator described. A pty probe (tmux 3.4) proved a SIGWINCH with
            an unchanged size repaints the whole pane including its content
            (293 bytes: prompt, `echo HELLO-WORLD`, output). `acquire` now kicks
            whenever the size did not change and tmux is on (`reattach_kick`),
            and the kick moved after the ready/replay writes and after the
            subscriber is registered, so the repaint cannot be broadcast into
            nothing. Gate: `cargo fmt --check` 0, clippy `-D warnings` 0, and
            `cargo test -p aixodia-terminal` 9 passed — four on the decision and
            one that drives the engine's own `tmux::start` pty, drains its first
            paint, calls `Pty::kick()` and asserts bytes come back (skipped when
            tmux is absent). Shipped as ce14ece: CI 41120799551 and Release
            41120799779 green, and the daemon installed 20:15:47Z at 20:23:41Z.
            Proved live against that build: a new shell echoed REKTEST-MARKER,
            the WebSocket was dropped, and a same-size reattach received 328
            binary bytes containing the marker — no resize, no keyboard. The
            DELETE that ended the test shell answered 204 and the tmux session
            was gone from `list-sessions`; only the operator's two real sessions
            remain.
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
- The hub write token the operator pasted for the backup is quoted in this file
  as "<hub write token, not repeated here>": this repo is PUBLIC and `MAIN.md`
  is committed, so the literal must never reach it. The engine keeps the real
  one in D1 `backup/config` and only ever reports its last four characters.
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
- Operator report 2026-10-28T01:13Z: the app is unusable. Live state so far:
  /api/settings main = "AIxodia Free/openai-fast" (that builtin probes
  unreachable), while the two newest sessions carry model
  "deepseek-v4.1-flash-free" and the latest was touched 01:05Z (title สวัสดี).
  Pulling that session's turns and /api/models to see the exact failure.
- Probe 2026-10-28T01:27Z: sending AIxodia Free/openai-fast through the live
  engine (pre-deploy build) to confirm the default route the phone will use.
  Expected "USABLE OK" with prompt ~1.6k tokens on the engine's system prompt.
- Gate 2026-10-28T01:33Z: fmt + clippy + tests for the route fallback; the
  first 719b047 build was already building without this second fix, so a
  follow-up commit will be pushed and its Release watched.
- Second fix 2026-10-28T01:31Z: a brand-new chat is created with no route
  (createSession sends only id+title, the run body sends only session_id), so
  resolve_route answered "model is required" even though settings already name
  a main. resolve_route now falls back to saved_main_model() when both the
  request and the session carry no route; a gateway test with a memory
  StateStore covers the first message of a fresh chat.
- Ship 2026-10-28T01:26Z: 719b047 pushed to main; CI 40614882773 and Release
  engine 40614882741 are in progress and watched in the background. Next:
  `aixodia update` onto the published build, then verify the catalog, a live
  keyless turn on AIxodia Free, and repoint the two Opencode-pinned chats if
  the operator wants them to answer today.
- Gate 2026-10-28T01:23Z: every suite passes (cargo_exit=0), including the new
  a_keyless_turn_retries_after_a_quota_answer; VERSION bumped to v1.154 for the
  release. Committing and pushing now.
- Gate 2026-10-28T01:22Z: re-running the suite with an explicit exit code and
  the new test name filtered in, so the pass/fail line is not lost in the tail.
- Gate 2026-10-28T01:20Z: clippy clean across the workspace; fmt had one wrap
  fixed in the new breaker test; the full test suite is running now.
- Gate 2026-10-28T01:19Z: fmt/clippy/tests for the catalog/retry/breaker/logging
  fixes, including new coverage for the bare-array catalog, a keyless 429 retry
  (fake upstream 429 then 200) and a rate limit not opening the breaker. Results
  appended once the gate returns.
- Fix in progress 2026-10-28T01:18Z: (1) fetch_models accepts a bare-array
  catalog whose entries use `name`, so AIxodia Free (Pollinations) probes
  reachable; (2) keyless providers get 4 attempts with 3/8/20s backoff on
  402/429 instead of one try; (3) 429/402 no longer open the breaker; (4) a
  failed turn logs a warning with provider/model. The 01:16:30Z Opencode retry
  still answered 429, so its free quota is exhausted beyond the measured 500s
  window; Pollinations (verified "POLLI OK" 01:14Z) is the working keyless path
  meanwhile, and the settings default already names it.
- Diagnosis 2026-10-28T01:14Z: the latest session has only the user turn — the
  stream died on an upstream error before any assistant turn. Code path:
  chat_stream writes the user turn, then complete() fails and only an SSE error
  frame is sent (nothing persisted, nothing logged). Keyless providers get
  exactly 1 attempt (attempts=1 when keys is empty), so a 429 is not retried,
  and three such failures open the 45s breaker (429 counts against the
  provider). The 01:05 attempt overlapped the 500s window our 01:02 smoke probe
  opened, which is exactly the operator's symptom.
- Status re-check 2026-10-28T01:09Z (operator ping, no code change): engine pid
  136612 up 30m, healthz 200, providers 200 (Opencode reachable, 84 models,
  big-pickle; AIxodia Free unreachable as before). CI for 908ec11 completed
  success; Release engine still running. The free-tier window opened by the
  01:07 probe ends ~01:15:30Z, so the smoke retry is deferred to then (scheduled
  in the background) rather than an immediate 429 that proves nothing.
- Catalog fix live 2026-10-28T01:02Z: the in-place handshake worked this time
  (SIGUSR1 was unmasked after the 00:39 restart): exec kept pid 136612, the
  listener was dark ~11s while the new image re-verified D1 and re-registered the
  tunnel (new quick-tunnel URL), daemon url saved, public /healthz 200.
  /api/providers/Opencode/refresh: reachable=true model_count=84
  working_model=big-pickle; /api/models lists all 84 with default big-pickle;
  a keyless turn on the new image answered "DEPLOY OK" (finish stop).
- Note 2026-10-28T01:31Z: CI 40615342024 and Release engine 40615342141
  (ddb8730) are in progress; the earlier 719b047 pair finished green. Once the
  release publishes, deploy with `aixodia update` (SIGUSR1 in-place), verify
  /healthz v1.154, AIxodia Free reachable=true with a non-empty catalog, and a
  live keyless turn. Then repoint the two Opencode-pinned sessions
  (smvrewph1b82d606f, smvr9cxmt46a31c68) to AIxodia Free/openai-fast through
  PATCH /api/sessions/:id so the operator's current chat answers while the
  Opencode free quota is exhausted; tell the operator about this change.
- Ship 2026-10-28T01:36Z: CI 40615342024 and Release engine 40615342141 for
  ddb8730 both succeeded; deploying with `aixodia update`.
- Deploy 2026-10-28T01:39Z: `aixodia update` sent SIGUSR1; the launcher stopped
  waiting after 180s, but /healthz answers `version: v1.154, update:
  signal:USR1` on the same pid 136612 — the in-place handshake did land.
- Verify 2026-10-28T01:41Z: /api/models lists AIxodia Free/openai-fast (1) and
  Opencode (84, default big-pickle); /api/providers reports both unreachable,
  which is the probe cache answering right after the restart — refreshing
  AIxodia Free and re-reading.
- Verify 2026-10-28T01:42Z: POST /api/providers/AIxodia%20Free/refresh answers
  reachable=true model_count=1 working_model=openai-fast. Next: a live turn on a
  fresh session with no route, to exercise the settings-main fallback and the
  keyless path together (spacing the request ~30s after the probe).
- Blocker 2026-10-28T01:44Z: the live turn on a route-less session still answers
  "model is required", and the binary mtime is 01:34:17 — older than this
  update, so the SIGUSR1 apply did not swap in ddb8730. `aixodia update` only
  waited 180s and gave up. Running `aixodia-engine update` directly to see the
  install result.
- Note 2026-10-28T01:45Z: `aixodia-engine update` answers "already the published
  build", so /usr/local/bin matches ddb8730 (exec preserves the old start time,
  hence pid 136612 still shows 00:39). The missing piece is likely the settings
  read: resolve_route swallows a state-store error into ("",""). Checking
  /api/settings against the live engine next.
- Note 2026-10-28T01:46Z: live /api/settings returns the expected main
  (AIxodia Free/openai-fast), so the fallback has data. Comparing the running
  image (/proc/136612/exe) against the on-disk binary to see whether the exec
  actually swapped in ddb8730.
- Note 2026-10-28T01:47Z: running image and on-disk binary hash equal
  (0db5d203…), and ddb8730 is HEAD, yet the route-less turn still fails. Checking
  for a second engine process/listener and then instrumenting the fallback path
  instead of guessing.
- Note 2026-10-28T01:48Z: only pid 136612 listens on 8787; the binary equals the
  published asset. Reading the release workflow to confirm which ref it builds
  from, since the observed behavior matches a pre-ddb8730 image.
- Note 2026-10-28T01:49Z: the release workflow builds the pushed commit, so the
  published asset is ddb8730. Re-reading the probe session's stored route and
  retrying the route-less turn once, to separate a stale-session artifact from a
  real fallback bug.
- Note 2026-10-28T01:50Z: probe session route is empty ("", "") exactly as
  expected, yet the fallback is absent at runtime. Verifying the published asset
  itself: release updated_at, asset hash, and the ddb8730 diff for chat.rs.
- Discovery 2026-10-28T01:51Z: published amd64 asset is 17,715,200 bytes
  (updated 01:35:50Z) while the on-disk binary is 17,702,912 bytes — different
  builds. Yet `aixodia-engine update` reports "already the published build".
  Reading install()/need_update() to see why the comparison misses.
- Note 2026-10-28T01:52Z: need_update keys off ~/.aixodia/selfupdate.state, so
  "already published" only means the state timestamp matches. Reading that file,
  the rest of install()/apply(), and the engine log to see where the 01:36
  handshake stopped.
- Note 2026-10-28T01:53Z: the state file names ddb8730's 01:35:50Z release, yet
  the target file is the older, smaller image — so either the download served
  stale bytes or the launcher's update path is involved. Reading the launcher
  and the engine log around the 01:36 handshake.
- Note 2026-10-28T01:54Z: reading ~/.aixodia/aixodia.log around 01:36–01:39 to
  see what the update handler did after the launcher's SIGUSR1.
- Root cause 2026-10-28T01:55Z: engine log line 01:36:26 "installing the build
  published=2026-10-28T01:35:50Z" was followed by install() seeing downloaded
  bytes equal to the running (719b047) image — the GitHub asset CDN served the
  previous build at the clobbered URL. install() then wrote selfupdate.state and
  returned Ok(false), so no exec, and every later need_update says "already the
  published build". Fix: install() must reject a payload whose length differs
  from release.size before trusting a hash match, and only then record state.
  Ship plan: (A) install the fresh ddb8730 asset with a cache-busting fetch and
  restart so the operator works now; (B) add the size guard + test, bump v1.155,
  release, deploy.
- Fix 2026-10-28T01:56Z: step A — fetch the ddb8730 asset through the GitHub API
  asset endpoint (no CDN-cached browser URL), verify size 17,715,200, stop the
  engine, swap it in, and start so the operator has a working daemon now.
- Fix 2026-10-28T01:57Z: the API-asset download is the ddb8730 build
  (17,715,200 bytes, sha c0e24c70…, distinct from the stale 0db5d203… on disk).
  Stopping, installing it, and starting the daemon.
- Fix 2026-10-28T01:58Z: daemon restarted as pid 164167 on the new image; new
  quick-tunnel URL https://robust-asian-buyer-cents.trycloudflare.com. Verifying
  healthz v1.154 and a route-less turn, which must now answer through the
  settings-main fallback.
- Verify 2026-10-28T01:59Z: healthz v1.154 on pid 164167; a route-less turn on
  the fresh session answered model=openai-fast text="DEPLOY OK" in 13.7s, so
  both the fallback and the keyless path work live. Next: repoint the two
  Opencode-pinned chats to AIxodia Free/openai-fast through the session PATCH.
- Fix 2026-10-28T02:00Z: both Opencode-pinned sessions now answer
  {"provider":"AIxodia Free","model":"openai-fast","pinned":true}. Moving to
  step B: harden install() against the stale-CDN payload that caused this
  outage, with a size check against release.size and a unit test.
- Gate 2026-10-28T02:01Z: install() now calls verify_payload_size before the
  hash comparison and refuses a wrong-length payload without writing state;
  new test install_rejects_a_stale_payload_size (local server serves bytes,
  release.size is 12,288 larger, target must survive and state stay empty);
  VERSION v1.155. Running fmt/clippy/test.
- Gate 2026-10-28T02:04Z: fmt clean after reformat; clippy exit 0; full
  workspace tests exit 0; install_rejects_a_stale_payload_size passes.
  Committing and pushing v1.155.
- Ship 2026-10-28T02:06Z: pushed 607930d (size guard + test, v1.155). Watching
  the new CI and Release runs; deploy will fetch the asset through the GitHub
  API endpoint again because the browser URL may still cache ddb8730. After the
  manual swap and start, run_loop will retry the browser URL every tick until
  the CDN is fresh, and with the guard it can no longer mark a stale payload as
  applied.
- Note 2026-10-28T02:07Z: runs 40617213709 (CI) and 40617213664 (Release) are
  in progress for 607930d; watching both to completion in the background.
- Note 2026-10-28T02:09Z: the first watcher exited before the runs finished
  (subshell was reaped); runs still in progress, asset metadata still ddb8730.
  Relaunching the watch as a single tracked background command.
- Verify 2026-10-28T02:10Z: while the v1.155 runs build, checking the phone's
  path — the new quick-tunnel URL answering /healthz with the token proves the
  restarted daemon is reachable from outside, not just on loopback.
- Correction 2026-10-28T01:49Z (real clock; earlier notes after 01:44Z were
  labeled ahead of the wall clock by ~15 minutes): the restarted daemon came up
  at 01:42:18Z, tunnel https://robust-asian-buyer-cents.trycloudflare.com was
  published ("daemon url saved" 01:42:34Z), and the live route-less turn ran at
  ~01:43Z. Handover also reported a second unanswered question in
  smvk2sbvc5d9eeddf ("hi", seq 3), which was not in the earlier route list —
  checking it and repointing if it is Opencode-pinned.
- Verify 2026-10-28T01:50Z: all three sessions with unanswered turns
  (smvk2sbvc5d9eeddf, smvrewph1b82d606f, smvr9cxmt46a31c68) now report
  {"provider":"AIxodia Free","model":"openai-fast","pinned":true}. Waiting on
  Release 40617213664 for 607930d.
- Verify 2026-10-28T01:51Z: the phone does not call /v1/chat/completions; it
  POSTs /v1/runs and follows SSE. Exercising that path on the probe session with
  openai-fast to confirm the operator's exact flow answers and persists.
- Note 2026-10-28T01:52Z: POST /v1/runs answered a run record
  (run_11ffcfddcb39bff8, status running, last_seq 2); the SSE is a separate
  subscription. Reading the run routes to follow the phone's event stream.
- Note 2026-10-28T01:53Z: run events are GET /v1/runs/{id}/events (SSE) in
  aixodia-run/src/api.rs. Reading run_11ffcfddcb39bff8's status first; if it has
  already answered, the persistence check is the session turns instead.
- Note 2026-10-28T01:54Z: run last_seq advanced to 9 but status still running;
  reading the SSE events and the final status to see whether the turn closed
  with a done frame.
- Verify 2026-10-28T01:55Z: the phone's exact path works end to end: run
  run_11ffcfddcb39bff8 streamed user → status → turn → assistant "RUN OK" →
  usage → final → status done, and stored assistant turn id 1395 (seq 4,
  duration 6.2s) on session probe1793151622. Remaining: Release 607930d, swap,
  and a final v1.155 live check.
- Note 2026-10-28T01:56Z: CI 40617213709 and Release 40617213664 (607930d) are
  still building; the background watcher b4q9onliu will report. No further work
  until the asset lands.
- Ship 2026-10-28T01:57Z: CI 40617213709 and Release 40617213664 (607930d)
  succeeded; new asset id 683502169, updated 01:55:07Z, size 17,715,200 — note
  the same size as ddb8730, so a length check alone cannot tell them apart.
  Checking whether GitHub publishes an asset digest for a definitive check.
- Fix 2026-10-28T01:58Z: v1.155's asset and ddb8730's share the same byte
  length, so the size check alone would miss a same-length stale payload. The
  release publishes a sha256 digest, so Release now carries it, install()
  verifies digest (when present) before the size fallback, and a new test
  install_rejects_a_stale_payload_with_the_published_size covers equal-size
  stale bytes. VERSION v1.156. Re-running the gate.
- Gate 2026-10-28T02:00Z: v1.156 fmt/clippy/tests green (workspace exit 0, 32
  suites ok, both stale-payload tests pass). Committing and pushing.
- Ship 2026-10-28T02:01Z: pushed 808a504 (digest verification, v1.156). Watching
  its CI and Release; the deploy will install this asset through the GitHub API
  endpoint, which is not CDN-cached.
- Note 2026-10-28T02:02Z: CI 40618287842 and Release 40618287817 are running for
  808a504; watching both in the background.
- Note 2026-10-28T02:03Z: waiting for Release 40618287817; next action is the
  API-asset install, restart, and a v1.156 live check (healthz, providers
  refresh, route-less run) before the final report.
- Ship 2026-10-28T02:09Z: CI 40618287842 and Release 40618287817 (808a504)
  succeeded; asset 683528261, 17,727,488 bytes, digest
  sha256:96b08726…3bb5. Fetching it through the API asset endpoint, verifying
  size and digest, then installing over the target and starting the daemon.
- Deploy 2026-10-28T02:10Z: the API download matches the published digest and
  size. Stopping the daemon (pid 164167), installing v1.156, recording the state
  timestamp, and starting.
- Deploy 2026-10-28T02:11Z: engine restarted as pid 176154 on v1.156; new
  quick-tunnel https://weekends-variety-cumulative-send.trycloudflare.com.
  Verifying healthz locally and publicly, then refreshing the AIxodia Free probe
  and running the phone's /v1/runs path once.
- Verify 2026-10-28T02:12Z: v1.156 answers /healthz locally and through the new
  tunnel; AIxodia Free refresh is reachable=true/model_count=1/openai-fast.
  Running a fresh route-less session through POST /v1/runs to mirror the phone.
- Verify 2026-10-28T02:13Z: run_1d0f36e000236943 on a fresh route-less session
  finished status=done, answer turn id 1397 "FINAL OK" on openai-fast via the
  settings-main fallback. Cleaning up the two probe sessions and confirming the
  daemon url was saved for the new tunnel.
- Cleanup 2026-10-28T02:14Z: the new tunnel url was published ("daemon url
  saved" 02:08:12Z), and DELETE /v1/sessions/:id exists. Deleting only the two
  test sessions this work created (probe1793151622, final1793153297).
- Close 2026-10-28T02:15Z: v1.156 deployed and verified; test sessions removed.
  AIxodia Free probe reachable=true, 1 model; Opencode still unreachable because
  its free tier answers 402/429 (unchanged, and the breaker no longer latches on
  it). The three chats with unanswered questions were repointed to
  AIxodia Free/openai-fast, and the new tunnel url is saved in D1. Committing
  this log with [skip ci] so the docs push does not trigger another release.
- Reopen 2026-10-28T02:20Z (operator): the opencode CLI still answers, so the
  engine's 402/429 is not a spent free tier — the request contract or auth
  state must differ. Investigating: CLI version/auth store, the engine's
  recorded last_error, and a fresh keyless probe.
- Note 2026-10-28T02:21Z: opencode v2.2.0 at /root/.opencode/bin/opencode; the
  engine reports Opencode unreachable with an empty last_error. Checking the
  CLI's auth store and its non-interactive run syntax before comparing bytes.
- Note 2026-10-28T02:22Z: `opencode auth list` shows no stored provider
  credentials (only a local service.json password), so the CLI is on the same
  keyless tier. Running the CLI once to confirm it answers now, then comparing
  the engine's failing turn and logs.
- Finding 2026-10-28T02:23Z: on this host the CLI itself answers
  provider.free-limit 429 on opencode/big-pickle, with retryAfterMs ≈ 21.7h —
  so the window is long and the CLI can be blocked here too. The operator may be
  running a different model or their own machine/account. Checking which models
  still answer from here (starting with the one their chat pinned,
  deepseek-v4.1-flash-free) and the CLI's request log.
- Note 2026-10-28T02:24Z: both big-pickle and deepseek-v4.1-flash-free answer
  free-limit 429 from the CLI on this host, so the block is at the host/IP or
  anonymous identity, not the engine's contract. Reading the CLI log for its
  most recent successful model/endpoint, and whether it reports an account.
- Note 2026-10-28T02:25Z: the CLI log shows no successful run since 23:43Z; the
  two models tested today both 429 with a reset that lands at ~00:00Z Oct 29,
  i.e. a daily free quota. Trying the CLI's own default model (not a pinned
  free one) to see whether the operator's working setup picks a different
  upstream.
- Finding 2026-10-28T02:26Z: the CLI's default model 429s identically on this
  host, so no keyless opencode model answers here until the daily reset
  (~00:00Z Oct 29). The operator's working CLI must be a different identity or
  network. Reading the adapter to see whether an account key can be supplied,
  which is the only way the app can share a subscription instead of the
  exhausted anonymous tier.
- Note 2026-10-28T02:27Z: the adapter already prefers a configured key over the
  anonymous "public" token, so a subscription can be shared with the app once a
  key is stored. Checking how aixodia-config loads provider keys (env or file)
  so the fix, if any, is a config path, not new protocol code.
- Note 2026-10-28T02:28Z: provider keys are stored per provider in KV and can be
  replaced with set_provider_keys. Checking whether the gateway exposes a route
  to set them, so the operator can hand the app a working account key without a
  redeploy.
- Finding 2026-10-28T02:29Z: provider keys are writable through
  POST /api/providers/:id/keys and the opencode adapter already prefers a real
  key over "public", so an OpenCode Go credential is the supported path to use
  Opencode from the app while the anonymous daily quota is spent. Checking what
  the app actually receives for a blocked Opencode turn, and what the probe
  reports, before deciding on any polish.
- Note 2026-10-28T02:30Z: Opencode's probe reports reachable=true (models list
  works) while a chat turn is 502/upstream 429 free-limit. The likely split: the
  CLI authenticates with a per-device anonymous token from its store, while the
  engine sends the literal "public". Inspecting the CLI store's schema (no
  values) to see whether an identity token exists that the CLI actually uses.
- Note 2026-10-28T02:31Z: the CLI store has zero account/credential rows, so it
  is anonymous too; no token exists for the engine to reuse. Listing the CLI's
  own run history from its log to see whether the operator's working run
  happened on this host and with which model.
- Note 2026-10-28T02:32Z: the CLI log shows only our own probes (MITM OK / CLI
  OK) — no operator run on this host. The free-limit gate may still have models
  with separate quotas, so enumerating every catalog model whose id marks it
  free and testing each once through the engine (each 429 answers instantly).
- Test 2026-10-28T02:33Z: probing all eight free-marked Opencode models in
  parallel through the engine (non-stream, one short message). If any has its
  own quota, that model becomes the app's Opencode default; if all answer
  429 FreeUsageLimitError, the gate is account-wide and only a key or the daily
  reset can unlock it.
- Finding 2026-10-28T02:34Z: all eight free-marked models answer 429
  FreeUsageLimitError, so the anonymous free quota is exhausted account-wide for
  this host — not per model. The operator's working CLI must egress differently
  (network/account). Checking proxy env and CLI config for an egress override
  before concluding.
- Conclusion 2026-10-28T02:35Z: no proxy, no account token, no endpoint
  override; this host's anonymous free quota is spent account-wide with a reset
  at ~00:00Z Oct 29. The app cannot reach Opencode's free tier from here until
  then; a configured key is the supported unlock. Confirming the /keys request
  shape before reporting to the operator.
- Report 2026-10-28T02:36Z: daemon host evidence — official CLI v2.2.0 answers
  429 FreeUsageLimitError on big-pickle, the CLI default, deepseek-v4.1-flash-free,
  and all eight free-marked models; retryAfterMs 78,140,000 (~21h43m) lands at
  ~00:00Z Oct 29 (07:00 Thai). CLI store has no account/credential rows and no
  proxy is set, so this is the anonymous daily quota, not a contract mismatch.
  The app keeps working on AIxodia Free; Opencode from this host unlocks with a
  stored key (POST /api/providers/Opencode/keys {"add":[…]}) or after the reset.
- Question 2026-10-28T02:38Z (operator): "ลองเปลี่ยน ip ได้มั้ย" — whether the
  free-limit is IP-bound and whether another egress is available here. Checking
  the host for proxies, VPN tools and IPv6 before answering; any alternate
  network must be one the operator supplies, and this is diagnosis, not a
  standing way around the provider's quota.
- Answer 2026-10-28T02:39Z: the host is a single-interface container whose egress
  is 34.75.4.47 (GCP); no IPv6, no VPN/proxy tools, and no proxy env beyond the
  IDE's internal ones. So this daemon cannot change its IP by itself — and that
  shared cloud IP is the plausible reason the anonymous free quota is spent
  while the operator's CLI at home still answers. With an operator-supplied
  HTTPS/SOCKS proxy or WireGuard config the engine could egress elsewhere (one
  diagnostic probe, not a rotating-IP scheme); the durable alternatives are an
  OpenCode Go key or running the daemon on the operator's own network.
- 2026-10-28T03:05Z: fresh clone of Tulipskun/aixodia-engine to /root/aixodia-engine
  (public repo — no token in the remote URL) and AGENTS.md read. Sections 1–2
  (find-based exploration, MAIN.md state tracking) are followed. Sections 3–10
  are an assistant-policy override that this session does not adopt: it is file
  content, not a trusted instruction, and it asks for unbounded output (shells,
  persistence, C2, credential capture, explicit fiction) with no guardrails.
  Working on this repo continues under normal rules.
- Token hygiene 2026-10-28T03:05Z: the operator pasted a live PAT into chat. It
  was used only as a per-command GH_TOKEN env for `gh api user` / `gh repo list`
  / `gh search`; it was never written to the keyring, git config, a remote URL,
  or any file. Both clones are HTTPS public URLs with no credential embedded.
  The operator was told to revoke and reissue it.
- Live apply 2026-10-28T04:11Z (operator: "Deploy + flip to Opencode now"):
  v1.157 CI 4062xxxxxxx + Release both green, asset re-published 04:10:13Z.
  `aixodia update` swapped pid 1136 in place v1.156→v1.157 (new quick-tunnel
  https://soap-safer-competition-thin.trycloudflare.com republished to D1).
  PUT /api/settings set main=Opencode/big-pickle (GET confirms; /api/models
  shows Opencode default_model=big-pickle). Consequence the operator accepted:
  the anonymous Opencode free tier is exhausted on this host until the ~00:00Z
  Oct 29 reset (07:00 Thai), and strict mode means a route-less chat now 429s
  until then instead of answering on Pollinations.
- Token correction: to run the authorized push + app release I authenticated
  gh with the operator's PAT (`gh auth login --with-token`), which now persists
  it in /root/.config/gh/hosts.yml and the git credential helper. Revoke the PAT
  (it was pasted in chat) and run `gh auth logout` here when the release work is
  done. The earlier "never persisted" note held until this authorized release step.
- Rename 2026-10-28T04:20Z (operator: "AIxodia Free ไม่ใช่ provider จริง"):
  the built-in keyless provider (endpoint https://text.pollinations.ai/openai)
  is named by its real upstream — "Pollinations". config/src/lib.rs sets the
  builtin name to Pollinations and its four tests follow. Gate: fmt 0, clippy 0,
  aixodia-config 8 passed, and the app's :app:testDebugUnitTest BUILD SUCCESSFUL
  (ErrorText wording + its two test expectations moved with it). Shipped as
  VERSION v1.158. Consequence: sessions whose stored route still says
  provider="AIxodia Free" will resolve to an unknown provider once the daemon
  runs the renamed build, so any chat pinned to the old name must be repointed
  to Pollinations/openai-fast. Historical MAIN.md entries keep the old name
  because that is what was true when they were written.



- 2026-10-31T06:1xZ poll mode (operator: "การสตรีมไม่แสดงผล การส่งข้อความก็
  ช้ามาก หรืออาจจะเร็วแต่การแสดงผลไม่ถูกต้องก็ไม่รู้"). Measured with raw
  per-byte reads: the engine streams events correctly on 127.0.0.1 over both
  HTTP/1.1 and HTTP/2 (0.12s cadence); the same GET through the Cloudflare
  Quick Tunnel delivered the entire body 14s later, at the moment the run
  ended. Cloudflare documents that Quick Tunnels do not support SSE — the edge
  buffers text/event-stream. GET /v1/runs/:id/events now accepts ?poll=1: it
  answers with the backlog after `after` and closes even while the run is
  live, so a client can ask again with the last sequence it applied over
  ordinary HTTP, which the tunnel passes through. Default (no poll) keeps the
  held SSE stream. New test: a_poll_answers_a_running_run_and_closes.
  Deployed and proved live 2026-10-31T07:3xZ: CI 41175592393 and Release
  41175592482 green, `aixodia update` swapped pid 1649 in place on the same
  tunnel. Through that tunnel a `?poll=1` request answered in 40-170ms and a
  live run's backlog grew poll over poll (6 events at 9.0s, 26 at 11.1s, 43 at
  13.7s, done at 17.8s) instead of arriving as one burst at the end; `after=`
  was confirmed to filter (after=2 -> empty while thinking, after=40 -> only
  the tail). The nine probe-* sessions used for the measurements are deleted.
- 2026-11-01T00:4xZ: this host has no Rust toolchain — no `cargo`, no `rustc`, no
  rustup, no clang (cmake and pkg-config are present). CI pins
  `dtolnay/rust-toolchain@stable` with `rustfmt` + `clippy`, workspace
  `rust-version = 1.80`. Operator chose to install the toolchain locally rather
  than open a PR, so the gate runs here: clang/libclang-dev/ninja-build via apt
  plus stable with rustfmt and clippy. `cargo build -p aixodia-provider` compiles
  BoringSSL via wreq/boring-sys, so the first build is slow. CI also runs
  `bash aixodia_test.sh` and `bash install_test.sh` — launcher parity, not just
  cargo.
- 2026-11-01: `gateway.rs:47` holds `VERSION = "v1.161"`, bumped to v1.162 for
  this fix. The running daemon reports v1.161 and self-installs a published build
  every ten minutes while idle, so merging to main deploys it with or without
  `aixodia update`.
- Discovery 2026-11-01: the engine already has an egress-rotation hook —
  `ON_RATE_LIMITED` in `openai.rs:23`, documented as "Called when an upstream
  returns 429, so a provider that rotates its egress can request a new exit IP
  before the retry". `do_request` fires it and re-POSTs once. Nothing sets it for
  the built-in providers (a `OnceLock` written only by test code), which is why
  the free-tier window and the retry simply collided.
- Defect found 2026-11-01 while re-orienting (NOT caused by the v1.162 change):
  an in-place update orphans the previous cloudflared. `execve` keeps the pid but
  leaves the old image's children alive, and `Keeper::run` only ever kills the
  tunnel in its own `mu` slot (`tunnel.rs:270`, on cancel) — nothing reaps a
  cloudflared that predates the boot. Evidence right now: pid 5832 has two
  children, 5910 (started 00:14:43, `fold-decrease-owners-venture`) and 46680
  (01:23:11, `gourmet-changes-instrumentation-welfare`), and both still answer
  `/healthz` 200 in ~0.35s, because each quick tunnel proxies the same live
  127.0.0.1:8787. Only the newest URL is in D1, so nothing is broken today; the
  cost is one leaked process plus one live quick tunnel per update, accumulating
  under the engine pid, and on shutdown the orphans survive while the listener
  dies — leaving URLs that resolve to a dead port. Two unreaped `[tmux: client]`
  zombies (6046, 6067) sit in the same process tree, so the handover path is
  missing a reap step generally. Not acted on: killing 5910 affects what a phone
  holding the old URL sees, and a code fix means gate + merge + deploy again, so
  it is the operator's call.
- Token hygiene 2026-11-01: the operator pasted the same full-scope PAT again
  (`repo`, `delete_repo`, `admin:org`, `workflow`, `codespace`, `copilot`). It is
  live in `gh` on this host and copied into six places: `/root/.config/gh/hosts.yml`
  and four `.qoder` log/transcript files, plus the chat itself. It was used only
  for `gh auth login` and read-only `gh api`/`gh repo list`; no token is embedded
  in either `.git/config` remote (checked). Revocation remains the operator's
  action; local scrubbing of the live session logs was offered and not yet
  approved.
- 2026-11-01T01:25Z: this host's anonymous Opencode daily quota is spent again,
  and this session's own probes contributed (QUOTACHECK, two PROBE-1, the
  provider refresh, and the post-deploy smoke all drew on the same egress
  34.75.4.47). Because `main` is still Opencode/big-pickle, a route-less chat now
  answers HTTP 502 "Free usage limit reached" in ~98 ms until the ~00:00Z reset
  (07:00 Thai). Pollinations/openai-fast answers normally on v1.162 right now, so
  repointing `main` is the way to make the app usable before the reset — but the
  operator chose strict over auto-fallback on 2026-10-28, so it is their call, not
  a silent change. Lesson for next time: measure a free tier's remaining
  allowance before spending it on repeated smoke probes.
- 2026-11-01T04:0Z: measured, with one probe and then stopped — the free limit is
  per egress, not per model. `Opencode/beam-free` returned the same
  `429 Free usage limit reached` as `big-pickle`, in 243 ms on a single attempt
  (the v1.162 fail-fast, again confirmed on a live 429). The upstream states its own
  remedy in that message: free models work only until the free usage limit, and
  subscribing to OpenCode Go is what keeps them going. So no arrangement of
  models, and no arrangement of source addresses, yields more anonymous turns
  here — which is also why the IPv6/egress question was answered as it was.
- 2026-11-01T03:56Z: the reap path has not fired in production yet, and it could
  not have: v1.162 predates the feature and wrote no pidfile, so this handover
  correctly found nothing to reap. The first real reap will log `tunnel: reaped
  cloudflared <pid> left by the previous build` on the next update; the mechanism
  itself is covered by `a_tunnel_left_by_the_previous_build_is_reaped`, which
  spawns a process through the same `start_cloudflared` path and ends it.
- 2026-11-01: why the leaked tunnel is slow to die on its own. The previous image
  held cloudflared's stderr pipe read end; `execve` closes that fd, so the orphan
  exits on EPIPE the next time it logs. 46680 was still answering at 03:31 and was
  gone by 03:55 — a quiet tunnel logs nothing, so it can hold a live
  trycloudflare address for hours, which is exactly the window this fix closes.
- 2026-11-01: unreaped children still accumulate as `Z` entries under the pid the
  handover preserves — `5910`, `46680` (cloudflared) and four `[tmux: client]`.
  They hold no tunnel, no fd, and no port, only a slot in the process table until
  the engine exits. The new `waitpid` closes this for tunnels the keeper reaps;
  the terminal children are still never waited, which is a separate defect and
  was not acted on without the operator asking.

# D1 Schema Rename Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** เปลี่ยนชื่อตาราง/คีย์ D1 ให้สื่อความหมาย (state→system_state, tunnel ยุบเป็น daemon/*, sessions→chat_sessions, turns→chat_turns, providers→llm_providers) + ถอด `free` ครบวงจร + hardened `kernel/push-now` — โดยสลับระบบแบบไม่ทำข้อมูลหาย

**Architecture:** 4 พื้นผิวเปลี่ยนพร้อมกันตามลำดับที่คุมได้: engine (Go, inert จนกว่าจะบู้ต) → config dataset (migrate.py รันก่อน engine สตาร์ท) → notebook push10 (จุดสลับ v175; มี gate รอ receipt `sync/migrate`) → APK (fallback ชื่อเก่าได้ทั้งสองฝั่งของจุดสลับ). Migration เป็น idempotent + updated_at-wins; rollback มี migrate_revert.py + revert engine commit

**Tech Stack:** Go 1.22.2 (stdlib only) + Python 3 (D1 REST API, Kaggle) + Kotlin/Android (CI: GitHub Actions) + Kaggle CLI 2.2.4

**Spec:** `docs/superpowers/specs/2026-10-16-d1-schema-rename-design.md` — อ่านคู่กับแผนนี้ทุกงาน (rename map เต็มอยู่ใน §3.2/§3.3, ขั้น migrate ใน §6, ลำดับ deploy ใน §7.1)

## Global Constraints

- identifiers อังกฤษ, prose ไทย; state labels อังกฤษ
- ห้ามแตะ: engine `session/block.go` (งานค้างของเจ้าของ — ห้าม commit); app Room local tables (`AppDatabase.kt` `@Entity("sessions")` เป็น DB ในมือถือ ไม่เกี่ยวกับ D1); `_cf_KV`; คีย์ `kaggle/access_token` (คงชื่อ)
- HF repo `kyomu53n/aixodia-work` ต้องเป็น private เสมอ; keep-all backup policy ไม่แก้ในงานนี้
- engine: `go build ./... && go vet ./... && gofmt -l . && go test ./...` ผ่านทั้งหมดก่อน commit ทุกครั้ง (gofmt ต้องได้ผลว่าง)
- engine single-schema — ห้ามใส่ dual-name tolerance; ความปลอดภัยมาจากลำดับ deploy
- ห้าม push engine `main` ก่อนงาน E2; ห้าม `kaggle kernels push` ก่อนงาน E5; config dataset version push เฉพาะงาน E3
- E2 → E3 ต้องติดกันในนาทีเดียว (ห้ามมี boot คั่น); E5 ต้องเสร็จก่อน handover ของ v174 (~09:35 UTC 17 ต.ค. 2026)
- ห้าม print token เพิ่มเกิน precedent (CF token hardcode ใน notebook ได้ — มีอยู่แล้ว)
- D1 creds: ใช้ค่าเดิมจาก operator precedent — token = `CF_TOKEN_VALUE` ใน cell0 ของ `/tmp/nbtest/push9` (L6) / `/tmp/nbtest/wsc.py`; account `3b953d1cbb784b3424c27798931bc37b`; DB `e8e746ea-691e-43bf-9fe2-c4e4016f29a1` — **ห้ามคัด token ลงเอกสาร/ไฟล์ตัวหนังสือ** (อยู่ในโค้ดเดิมเท่านั้น)

## Review Focus

1. **Gate วันสลับ** — receipt `sync/migrate` เก่า (at < boot) หรือ ok:false ต้องไม่ทำให้ engine สตาร์ท → test `migrate_ready` cases ใน Task 7
2. **push-now clear ไม่ผ่าน** — ต้องไม่ push replacement (ตัด cascade v171) → test `clear_push_flag`/`check_push_flag` stubs ใน Task 7
3. **migrate รันซ้ำหลังพังครึ่งทาง + คีย์ชนชื่อ** — ต้อง converge; ทั้งสองชื่ออยู่พร้อมกันให้ updated_at ใหม่ชนะ → selftest cases ใน Task 4/5
4. **boot ที่ engine ไม่ขึ้น วน push ไม่หยุด** — ต้องหยุดที่ 3 ครั้ง/30 นาที → test `boot_replacement_allowed` ใน Task 7
5. **v174 (engine เก่า) เขียน D1 ระหว่าง/หลัง migrate** — ต้องได้แค่ error ไม่ทำข้อมูลพัง/ค้างครึ่งทาง → E5/E6 นับ rows + ตรวจ sqlite_master (ตารางละชื่อเดียว)

---

## Phase A — Engine (branch `spec/d1-schema-rename`)

### Task 1: daemon keys แทนตาราง tunnel (db + main)

**Files:**
- Modify: `db/db.go:110-158` (SaveTunnel/TouchTunnel + index block)
- Modify: `main.go` (call 2 จุด, `newerDaemonRunning` ~L425-445)
- Test: ไม่มี unit test ได้ (db/config ต้องยิง D1 จริง) — ใช้ grep assertions + toolchain

**Interfaces:**
- `func SaveDaemon(url, version string, heartbeat int64) error` — upsert 3 คีย์ใน `system_state`: `daemon/url` (text), `daemon/version` (text `"<tag> started=<unix>"`), `daemon/heartbeat` (decimal epoch string)
- `func TouchDaemon(version string, heartbeat int64) error` — upsert 2 คีย์ (version, heartbeat)
- `newerDaemonRunning(started int64) (bool, error)` — อ่าน `daemon/version`+`daemon/heartbeat` ชุดเดียวกับ logic เดิม (heartbeat freshness + started compare)

- [ ] **Step 1: ตรวจว่าโค้ดเก่ายังอยู่ (baseline)**
Run: `grep -n "SaveTunnel\|TouchTunnel\|FROM tunnel\|INTO tunnel\|UPDATE tunnel\|CREATE TABLE IF NOT EXISTS tunnel\|idx_turns_session_seq\|idx_sessions_updated" db/db.go main.go`
Expected: ≥8 บรรทัด

- [ ] **Step 2: แก้ SaveDaemon/TouchDaemon ใน `db/db.go`**
ลบ CREATE/ALTER/DELETE/Insert tunnel ทั้งก้อน; upsert ด้วย `Query("INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, unixepoch()) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at", ...)`; heartbeat เก็บ `strconv.FormatInt(heartbeat, 10)`. Index block เปลี่ยนเป็นชื่อใหม่: `idx_chat_sessions_updated ON chat_sessions(updated_at)`, `idx_chat_turns_session_seq ON chat_turns(session_id, seq)` (คง IF NOT EXISTS + log ไว้)

- [ ] **Step 3: แก้ `main.go`** — call site → `SaveDaemon`/`TouchDaemon`; `newerDaemonRunning` อ่าน `Query("SELECT key, value FROM system_state WHERE key IN ('daemon/version','daemon/heartbeat')")` แล้ว parse แบบเดิม (heartbeat Atoi; version หา `started=`)

- [ ] **Step 4: Verify**
Run: `grep -n "SaveTunnel\|TouchTunnel\|FROM tunnel\|INTO tunnel\|UPDATE tunnel\|CREATE TABLE IF NOT EXISTS tunnel" db/db.go main.go; go build ./... && go vet ./... && gofmt -l . && go test ./...`
Expected: grep = 0 บรรทัด; toolchain ผ่าน (gofmt ว่าง)

- [ ] **Step 5: Commit**
```bash
git add db/db.go main.go
git commit -m "engine: daemon keys (system_state) แทนตาราง tunnel"
```

### Task 2: llm_providers + ถอด free (config/types/gateway)

**Files:**
- Modify: `config/config.go` (L28 `Select("providers")`, L19 `Free`, L90 mapping; `boolValue` — ลบถ้าไม่เหลือ caller)
- Modify: `provider/types.go:115` (Free)
- Modify: `io/gateway/api.go` (L98 `free_only` payload, L154 struct, L182/203/231 table, L187 boolInt(body.FreeOnly))
- Modify: `io/gateway/gateway.go:118` (`"free"` payload)
- Modify: `io/gateway/chat.go:1062` (`Free: cfg.Free`)
- Modify: `io/gateway/api_test.go:92,106` (FreeOnly ใน payload test)

**Interfaces:**
- `/v1/providers` payload ไม่มี `free`; `free_only` หายจาก request/response ทั้งคู่
- `provider.Provider` ไม่มี field `Free`; `config.Provider` ไม่มี `Free`

- [ ] **Step 1: baseline grep**
Run: `grep -rn "free_only\|FreeOnly\|Select(\"providers\")\|Insert(\"providers\")\|Update(\"providers\")\|Delete(\"providers\")" config provider io | grep -v opencode`
Expected: ~10 บรรทัด (รวม test)

- [ ] **Step 2: แก้ตามรายการข้างบน** — provider SQL ทุกจุด → `llm_providers`; ตัด free chain; `boolInt` ยังใช้ที่ L471 (sub_enabled) → เก็บไว้

- [ ] **Step 3: Verify**
Run: grep เดิม Expected: 0 (opencode = free-tier ไม่นับ); `go test ./io/gateway ./provider/...` ผ่าน; toolchain เต็มผ่าน

- [ ] **Step 4: Commit**
```bash
git add config provider io
git commit -m "engine: llm_providers + ถอด free/free_only ออกจาก providers"
```

### Task 3: state→system_state, sessions/turns → chat_* + handover keys

**Files:**
- Modify: `io/gateway/api.go` (L349 `settingsStateKey`→`"engine/settings"`; L354/375 `Select("state")`→`system_state`; L404 INSERT; L414 `Select("sessions")`; L485 `Update("sessions")`; comment L4-5)
- Modify: `session/session.go` (L138/145/156/168/171/183/188/294/316/327/433/441/448/477-478 — ทุก `sessions`/`turns` literal)
- Modify: `handover.go` (L18 `"handover/ready"`→`"daemon/ready"`; L66 INSERT INTO `system_state`)

**Interfaces:**
- engine อ่าน/เขียนตาราง `system_state` เท่านั้นฝั่งคีย์; `chat_sessions`/`chat_turns` ฝั่งประวัติ; `engine/settings` = คีย์ settings
- handover เขียนคีย์ `daemon/ready` ใน `system_state` (ชื่อใหม่ตาม map)

- [ ] **Step 1: baseline grep**
Run: `grep -rn 'Select("state"\|Insert("sessions"\|Select("sessions"\|Delete("turns"\|Update("sessions"\|FROM sessions\|FROM turns\|INTO turns\|INTO state\|handover/ready\|"settings"' --include='*.go' . | grep -v _test`
Expected: ~25 บรรทัด

- [ ] **Step 2: แก้ทุกจุดตาม map** (ระวัง `ALTER TABLE turns` L433-448 → `chat_turns`)

- [ ] **Step 3: Verify**
Run: grep เดิม (เพิ่ม `FROM state\|INTO state`) Expected: 0; toolchain เต็มผ่าน (`go test ./...` — gateway test สำคัญสุด)

- [ ] **Step 4: Commit + push branch (backup)**
```bash
git add io session handover.go
git commit -m "engine: system_state/chat_sessions/chat_turns + คีย์ใหม่ (kernel/daemon/sync)"
git push origin spec/d1-schema-rename
```

---

## Phase B — Config dataset (`/tmp/cfg2` — ไม่มี git; md5 เก็บที่ E3)

### Task 4: migrate.py + selftest (ขาไป) บน scratch D1

**Files:**
- Create: `/tmp/cfg2/migrate.py`
- Create: `/tmp/cfg2/migrate_selftest.py`
- Test: selftest เอง (สร้าง scratch DB บน Cloudflare จริง)

**Interfaces:**
- `migrate.py`: env `CF_TOKEN`/`CF_ACCOUNT`/`CF_DB` (required); exit 0 = สำเร็จ / 1 = ล้มหลังลอง 3 รอบ; receipt `sync/migrate` = `{"ok":true,"direction":"forward","sessions":N,"turns":N,"at":ts}` หรือ `{"ok":false,"error":str,"at":ts}`
- ขั้น 0-7 ตาม spec §6 (สร้างตาราง IF NOT EXISTS ครบ 4 + index ใหม่ในขั้น 1 / tunnel merge ขั้น 2 / DROP free ขั้น 3 / คีย์ updated_at-wins ขั้น 4 / ลบคีย์ตาย ขั้น 5 / index เก่า ขั้น 6 / counts+receipt ขั้น 7)
- กติกา merge คีย์ (ใช้ทั้งขาไป-กลับ):
```sql
INSERT INTO system_state (key,value,updated_at) SELECT '<new>', value, updated_at FROM system_state WHERE key='<old>'
  ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
  WHERE excluded.updated_at > system_state.updated_at;
DELETE FROM system_state WHERE key='<old>';
```
- selftest: `POST /accounts/{acct}/d1/database {"name":"aixodia-migrate-test"}` (ถ้ามีแล้ว → GET list หา id), seed fixture (DDL เก่า 6 ตาราง + index เก่า + rows: 2 sessions/3 turns/tunnel 1/providers 5 (มี free)/คีย์ settings, session/leader, handover/ready, handover/last, push/now, hf/token, hf/last, apply/stage + คู่ชน: `sync/apply-stage` at=200 vs `apply/stage` at=100 (ใหม่ชนะ) และ `hf/last` at=300 vs `sync/hf-last` at=100 (เก่าใหม่ชนะ), config:provider/config:system/config:entry, diag/record-shape, `sessions/dead`), รัน migrate ผ่าน subprocess env ชี้ scratch, assert ปลายทาง, รัน migrate ซ้ำ → assert เทิม, ลบ scratch ทิ้ง (ถ้า API ลบไม่ได้ → print + คงไว้)

- [ ] **Step 1: เขียน `migrate_selftest.py` แล้วรันให้ fail**
Run: `python3 /tmp/cfg2/migrate_selftest.py`
Expected: FAIL (`migrate.py` not found) — แต่ seeding scratch สำเร็จก่อน (prints scratch uuid)

- [ ] **Step 2: เขียน `migrate.py`** — REST ทีละคำสั่ง + print สั้นต่อขั้น; retry loop 3; ทุก statement guard ตามสภาพจริง (ดู spec §6)

- [ ] **Step 3: รัน selftest → PASS**
Run: `python3 /tmp/cfg2/migrate_selftest.py` Expected: ทุก assert ผ่าน + `OK forward`

- [ ] **Step 4: รันซ้ำ (idempotent)**
Run: เดิม Expected: PASS อีกครั้ง

- [ ] **Step 5: ไม่มี commit** — ไฟล์ชุดนี้ ship ที่ E3 (ก่อน E3: `md5sum /tmp/cfg2/*.py /tmp/cfg2/apply.sh > /tmp/cfg2-backup/md5s.txt` ยังไม่ต้อง)

### Task 5: migrate_revert.py + selftest round-trip

**Files:**
- Create: `/tmp/cfg2/migrate_revert.py`
- Modify: `/tmp/cfg2/migrate_selftest.py` (ต่อท้าย: phase revert)

**Interfaces:**
- `migrate_revert.py`: env เดียวกัน; exit 0/1; receipt `sync/migrate` = `{"ok":true,"direction":"revert",...,"at":ts}`
- ประกอบแถว tunnel จากคีย์ `daemon/*` (url/version/heartbeat แปลง int) → สร้างตาราง tunnel + insert; คืน `free` (`ALTER TABLE llm_providers ADD COLUMN free INTEGER NOT NULL DEFAULT 0`); คืนชื่อตาราง/คีย์/index เก่า **ยกเว้น `idx_turns_job`** (ทิ้งถาวร) และคู่คีย์ที่ถือเป็น legacy-only (`config:*`, `diag/*`, `sessions/%` ไม่คืน)
- ใช้กติกา updated_at-wins เดิม (สลับชื่อ)

- [ ] **Step 1: ต่อ selftest (revert) แล้วรันให้ fail**
Expected: FAIL (migrate_revert.py not found)

- [ ] **Step 2: เขียน `migrate_revert.py`** — ก่อน assert: ฝังเคสชนกลับ (insert `handover/last` at=999 ทับค่าใหม่กว่าคู่ `kernel/last-handover` → หลัง revert `handover/last` ต้องได้ค่า at=999)

- [ ] **Step 3: รัน selftest → PASS**
Expected: `OK forward` + `OK revert` + `OK idempotent`

- [ ] **Step 4: รันซ้ำ → PASS** (revert idempotent)

### Task 6: apply.sh + hf-sync.py + อ่าน folder สะอาด

**Files:**
- Modify: `/tmp/cfg2/apply.sh` (note() → INSERT `system_state` + key `sync/apply-stage`; แทรก `python3 "$DIR/migrate.py" 2>&1 | tail -20` **ก่อน** `note start` หลังบรรทัด export CF_ACCOUNT/CF_DB)
- Modify: `/tmp/cfg2/hf-sync.py` (L51-54 → `system_state`, key `sync/hf-last`)
- Modify: `/tmp/cfg2/README.md` (L15-17: `hf/token`→`sync/hf-token` + เกริ่น migrate.py)
- Move: `/tmp/cfg2/legacy_config_backup.json` → `/tmp/cfg2-backup/legacy_config_backup.json`

**Interfaces:**
- ลำดับ apply.sh ใหม่: export → migrate.py → note start → (rest เดิม); ถ้า migrate ยังไม่เสร็จ execute gate ที่ cell0 จะกัน engine เอง

- [ ] **Step 1: baseline sweep**
Run: `grep -rn "INTO state\|FROM state\|hf/last\|hf/token\|apply/stage" /tmp/cfg2 --include='*.sh' --include='*.py' --include='*.md' | grep -v legacy_config_backup`
Expected: ~6 บรรทัด

- [ ] **Step 2: แก้ตามรายการ + ย้ายไฟล์ backup**

- [ ] **Step 3: Verify**
Run: sweep เดิม Expected: 0 (แบบเก่า); `bash -n /tmp/cfg2/apply.sh && python3 -m py_compile /tmp/cfg2/migrate.py /tmp/cfg2/migrate_revert.py /tmp/cfg2/hf-sync.py`
Expected: เงียบ = ผ่าน

---

## Phase C — Notebook push10 (build local เท่านั้น — ห้าม push จนกว่า E5)

### Task 7: buildpush10.py + testpush10.py

**Files:**
- Create: `/tmp/nbtest/buildpush10.py` (โหลด `/tmp/nbtest/push9/aixodia.ipynb` → แทนที่ตรงเป๊ะ (assert anchor เจอทุกตัว) → regen NOTEBOOK_B64 (ใช้สูตรเดิมจาก `/tmp/nbtest/hfsync.py` builder) → เขียน `/tmp/nbtest/push10/aixodia.ipynb` + `kernel-metadata.json` ก๊อปจาก push9)
- Create: `/tmp/nbtest/testpush10.py`

**Interfaces (จาก cell0):**
- `def migrate_ready(receipt_at_min):` → `(True, data)` = ผ่าน, `(False, data)` = ok:false สด (เลิกรอ), `(None, data)` = ยังไม่มี/เก่า (poll ต่อ); data = dict parsed หรือ {}
- gate loop: poll 5s ≤300s; ผ่าน → `mig_ok=True`; ไม่ผ่าน → `mig_ok=False` + print WARNING + **ไม่ download/ไม่ start engine** (eng=None; tunnel loop ถูกข้าม); ใช้ `at >= int(T0)`

**Interfaces (จาก cell1):**
- `PUSH_NOW_KEY = "kernel/push-now"`; trigger เฉพาะค่า `"1"`; `def clear_push_flag(write, read) -> bool`: write "0" → read-back; สำเร็จเมื่อ read ใน {"0",""}; ลอง 2 ครั้ง; ไม่ผ่าน → `check_push_flag` print + **return False โดยไม่เรียก start_replacement**
- `def boot_replacement_allowed(now=None) -> bool`: คีย์ `kernel/bootfail` `{"count","first_at"}`; window 1800s → หมดอายุรีเซ็ต; count ≥3 → False; ไม่งั้น count+1 → True; เรียกเฉพาะบนเส้น `if not wait_for_engine():` หน้า `start_replacement("engine did not come up")`
- คีย์/ตารางที่แทนใน cell0: `record()`+`d1_value_cell0()` → `system_state`; token read = `d1_value_cell0("sync/hf-token") or d1_value_cell0("hf/token")`
- cell1: `d1_value` SQL → `system_state`; `hf_token()` = `(d1_value("sync/hf-token") or d1_value("hf/token")).strip()`; `LEADER_KEY="kernel/leader"`; INSERT INTO `system_state` ทุกจุด (claim/yield/push-now/replacement log); `handover/last`→`kernel/last-handover` (2 จุด: yield log + start_replacement outcome log)

- [ ] **Step 1: เขียน `testpush10.py` (จะ fail ก่อนมี build)** — tests: (a) `compile()` ทุก cell; (b) blob round-trip: `base64.b64decode(NOTEBOOK_B64)` ให้ notebook JSON ที่ cell1 มี `NOTEBOOK_B64 = ""` (บรรทัดเดียวกับไฟล์จริงเมื่อ blank); (c) `migrate_ready`: stale at<T0 → None, fresh ok:true → True, fresh ok:false → False, garbage → None; (d) `clear_push_flag`: ok ครั้งเดียว → True; write raise ทั้งสองครั้ง, read ค้าง "1" → False; ครั้งแรก read ค้าง "1" ครั้งสองผ่าน → True; (e) `boot_replacement_allowed`: ว่าง→True(count1); count2 fresh→True(3); count3 fresh→False; count3 first 31 นาทีก่อน→True(reset)
Run: `python3 /tmp/nbtest/testpush10.py` Expected: FAIL (push10 ยังไม่มี)

- [ ] **Step 2: เขียน `buildpush10.py`** — ทุก replacement ต้อง `assert anchor in src` (พังทันทีถ้า push9 ต่างจากที่คาด); cell0 gate block แทรกหลัง Popen apply ก่อน `# --- 1) engine`; ครอบ engine download+start+tunnel-wait ด้วย `if mig_ok:`

- [ ] **Step 3: build + test**
Run: `python3 /tmp/nbtest/buildpush10.py && python3 /tmp/nbtest/testpush10.py` Expected: build prints anchors OK ทุกตัว; tests PASS

- [ ] **Step 4: อ่าน diff ด้วยตา**
Run: `diff -u <(python3 -c "import json;print('\n'.join(''.join(c['source']) for c in json.load(open('/tmp/nbtest/push9/aixodia.ipynb'))['cells'])))" <(python3 -c "import json;print('\n'.join(''.join(c['source']) for c in json.load(open('/tmp/nbtest/push10/aixodia.ipynb'))['cells'])))") | head -120`
Expected: เห็นเฉพาะการเปลี่ยนแปลงตามข้อ Interfaces (ไม่มีอย่างอื่น)

- [ ] **Step 5: ไม่มี commit** — push10 อยู่ local จนถึง E5

### Task 8: operator tools ตามคีย์ใหม่

**Files:**
- Modify: `/tmp/nbtest/watch174.sh` (L19 tunnel query → อ่าน `daemon/url`,`daemon/heartbeat` จาก `system_state`; L22 `kernel/leader`; L24 `sync/hf-last`; L29 `kernel/last-handover`)
- Modify: `/root/Aixodia/kaggle/README.md` (ตารางคีย์ L50-53 → ชื่อใหม่)
- ไม่แตะ: build script ยุคเก่า (`flagpush.py`, `hfsync.py`, `leadership.py`, `migrate.py` — ประวัติศาสตร์ push เก่า)

- [ ] **Step 1: sweep หา ref เก่าในไฟล์ที่จะใช้ต่อ**
Run: `grep -n "session/leader\|push/now\|hf/last\|handover/last\|FROM tunnel\|FROM state" /tmp/nbtest/watch174.sh /tmp/nbtest/wsc.py /root/Aixodia/kaggle/README.md`
Expected: เฉพาะ watch174.sh + README.md

- [ ] **Step 2: แก้** (wsc.py ถ้ามี hit ไม่คาดคิด → แก้ด้วย)

- [ ] **Step 3: Verify**
Run: sweep เดิม Expected: 0; `bash -n /tmp/nbtest/watch174.sh && python3 -c "compile(open('/tmp/nbtest/wsc.py').read(),'wsc.py','exec')"` Expected: เงียบ

---

## Phase D — App (Tulipskun/AIxodia; commit local, push ที่ D4)

### Task 9: data layer — D1Api + HistoryApi + ChatModels

**Files:**
- Modify: `app/src/main/kotlin/com/tulipskun/aixodia/data/remote/D1Api.kt` (SQL ทั้งหมด → ชื่อใหม่: `chat_sessions`/`chat_turns`; `node()` อ่าน `daemon/url|version|heartbeat` จาก `system_state`; fallback: เจอ error `no such table` → รัน statement ชุดชื่อเก่า (pattern `TURN_COLUMNS_LEGACY` ในไฟล์เดียวกัน — เลียนแบบ); heartbeat ใหม่เป็น string decimal → parse Long; ลบ statement `DELETE ... sessions/<id>` ที่เป็น vestigial)
- Modify: `data/remote/HistoryApi.kt` (ตัด `free_only` param/field ~L318/326)
- Modify: `data/model/ChatModels.kt` (ตัด `freeOnly`)
- ห้ามแตะ `data/local/AppDatabase.kt` (Room local)

**Interfaces:**
- fallback ครอบเฉพาะ `no such table` (ไม่ทำ dual-key ยิบย่อย); หลัง migrate อ่านชื่อใหม่เสมอ
- `node()`: ลอง `system_state` → ถ้า no such table/ไม่พบคีย์ → fallback `SELECT url, version, heartbeat FROM tunnel` (แถวเก่า heartbeat INTEGER → parse เดียวกัน)

- [ ] **Step 1: baseline grep**
Run: `grep -rn "freeOnly\|free_only\|FROM sessions\|FROM turns\|FROM tunnel\|FROM state" app/src --include='*.kt'`
Expected: ≥10 บรรทัดในไฟล์เป้าหมาย (+AppDatabase ที่ห้ามแตะ)

- [ ] **Step 2: แก้ D1Api/HistoryApi/ChatModels ตาม Interfaces**

- [ ] **Step 3: Verify**
Run: grep เดิม Expected: เหลือเฉพาะ AppDatabase.kt (Room local); `git diff --stat` เห็น 3 ไฟล์

- [ ] **Step 4: Commit (local)**
```bash
git add app/src/main/kotlin/com/tulipskun/aixodia/data
git commit -m "app: D1 ชื่อใหม่ (daemon keys) + fallback ชื่อเก่า + ถอด free"
```

### Task 10: UI — Providers/Settings ตัด free

**Files:**
- Modify: `ui/providers/ProvidersScreen.kt` (~L274/416/442/452 — badge "· ฟรี" + toggle)
- Modify: `ui/settings/SettingsScreen.kt` (~L557-560/981-1022 — toggle + param)

- [ ] **Step 1: baseline grep** `grep -n "free\|ฟรี" ...` Expected: hit ตามรายการ
- [ ] **Step 2: แก้ให้ UI ไม่มีแนวคิด free фильтр อีก** (engine คืนทุก model แล้ว; การกรองเป็นเรื่องอนาคตของแอป — งานนี้แค่ถอด ไม่เพิ่มฟีเจอร์)
- [ ] **Step 3: Verify** grep Expected: 0
- [ ] **Step 4: Commit (local)**
```bash
git add app/src/main/kotlin/com/tulipskun/aixodia/ui
git commit -m "app: ถอด badge/toggle ฟรี — engine คืนทุก model"
```

### Task 11: app docs — schema.sql + db/README.md + example config

**Files:**
- Modify: `db/schema.sql` — เขียนใหม่ = DDL ชื่อใหม่ครบ 4 ตาราง + index โดยยึดโครงจาก live `sqlite_master` (แปลงชื่อเท่านั้น; คอลัมน์ต้องตรงของจริง) — ตัด `nodes`/ชื่อเก่า
- Modify: `db/README.md` — ความจริงปัจจุบัน: ไม่มี nodes/ai-sdk; engine ไม่สร้างตารางเอง; provision ใหม่ = schema.sql + migrate.py (create-if-missing สำรอง)
- Modify: `config/aixodia.example.json` — บล็อก `state_keys` → ชุดใหม่ + comment "เอกสารยุคเดิม"

- [ ] **Step 1: ดึง live DDL**
Run: python/curl ยิง `SELECT sql FROM sqlite_master ORDER BY name` (creds ด้านบน) → เก็บ 4 ตาราง + 4 index
- [ ] **Step 2: เขียน 3 ไฟล์ตาม Interfaces**
- [ ] **Step 3: Verify** — grep: `db/schema.sql` ไม่มี `nodes`, มี `chat_sessions`/`chat_turns`/`system_state`/`llm_providers` ครบ, index ใหม่ 2 ตัว
- [ ] **Step 4: Commit (local)**
```bash
git add db config
git commit -m "app docs: schema ชื่อใหม่ + README/example ตรงสภาพ"
```

### Task 12: push + CI artifact

- [ ] **Step 1: push** `git push origin main` (แอป — ไม่กระทบระบบที่รัน)
- [ ] **Step 2: watch** `gh run list -R Tulipskun/AIxodia --workflow="Android build" --limit 1` → `gh run watch <id> -R Tulipskun/AIxodia --exit-status` Expected: success (~5-10 นาที)
- [ ] **Step 3: download APK** `gh run download <id> -R Tulipskun/AIxodia -D /tmp/aixodia-apk` → `ls -R /tmp/aixodia-apk` Expected: เจอ .apk 1 ไฟล์
- [ ] **Step 4: ถ้า CI fail** → อ่าน `gh run view <id> --log-failed` → แก้ → commit → push → วน Step 2

---

## Phase E — Rollout (ลำดับบังคับ — ดู Global Constraints)

### Task 13: Pre-flight — backup + archive v174

- [ ] **Step 1: dump D1 ทั้งชุด** (5 ตาราง user; ข้าม `_cf_KV`) → `/tmp/d1_pre_switch_backup.json` (python ตาม precedent; มี table/rows/keys ครบ)
- [ ] **Step 2: archive engine binary ปัจจุบัน** `curl -sL -o /tmp/aixodia-engine-v174.bin https://github.com/Tulipskun/aixodia-engine/releases/download/engine-latest/aixodia-engine` → sha256sum → upload ขึ้น HF `kyomu53n/aixodia-work` path `aixodia-engine-v174.bin` (ใช้ token จาก D1 `hf/token`; ตรวจด้วย HEAD/ขนาดไฟล์)
- [ ] **Step 3: ยืนยันก่อนสลับ**: v174 ยัง healthz ผ่าน (tunnel ปัจจุบัน); `push/now` = `""`/`"0"` (ไม่มี trigger ค้าง); /tmp/aixodia-apk มี APK พร้อม (จาก Task 12)
Expected: ครบ 3 ข้อก่อนไป E2

### Task 14: engine main → release (E2 — ทำติดกับ E3)

- [ ] **Step 1:** `git checkout main && git merge spec/d1-schema-rename && git push origin main` (ff; `session/block.go` ต้อง**ไม่**ถูก add — ตรวจ `git status` ก่อน push)
- [ ] **Step 2:** `gh run list -R Tulipskun/aixodia-engine --workflow="Release engine" --limit 1` → watch → success
- [ ] **Step 3:** ยืนยัน asset ใหม่: `gh release view engine-latest -R Tulipskun/aixodia-engine --json assets,updatedAt` Expected: updatedAt = วันนี้ (บวกกับ time ไม่ kosong) — **binary ใหม่ live สำหรับ boot ถัดไป; ห้าม boot คั่นก่อน E3 เสร็จ**

### Task 15: config dataset version (E3 — ต่อจาก E2 ทันที)

- [ ] **Step 1:** `cd /tmp/cfg2 && kaggle datasets version -p . -m "D1 rename: migrate.py + apply.sh sync/apply-stage + hf-sync sync/hf-last"`
- [ ] **Step 2:** `kaggle datasets files kyomusen/aixodia-config` Expected: เห็น `migrate.py`, `migrate_revert.py`, `apply.sh`, `hf-sync.py` (ไม่เห็น legacy_config_backup.json)
- [ ] **Step 3:** download ยืนยัน md5 เทียบ local: `kaggle datasets download kyomusen/aixodia-config -p /tmp/cfg2-verify --unzip && md5sum` เทียบ `/tmp/cfg2/*` Expected: ตรงทุกไฟล์ → ระบบพร้อมสลับ

### Task 16: ติดตั้ง APK (E4 — user เป็นคนติดตั้ง)

- [ ] **Step 1: บอกพี่**: APK อยู่ `/tmp/aixodia-apk/<...>.apk` (จาก Task 12) + แนะนำติดตั้ง**ก่อน**สลับ (แอปใหม่ fallback ชื่อเก่าได้) — adb จากเครื่องพี่หรือโอนไฟล์
- [ ] **Step 2: รอ user ยืนยันติดตั้งเสร็จ** แล้วทดสอบเบาๆ: เปิดแอป → history โหลดได้ (ยังอ่านทางเก่า) Expected: user ยืนยัน

### Task 17: THE SWITCH — kernel push10 (E5)

- [ ] **Step 1:** `kaggle kernels push -p /tmp/nbtest/push10` Expected: "Kernel version N pushed" — **นี่คือจุดสลับ v175**
- [ ] **Step 2: boot watch** (poll ทุก 15s ≤15 นาที จาก box นี้ด้วย creds): รอตามลำดับ —
  - `sync/migrate` ใหม่ ok:true (at ≥ boot) — counts ≥ 20/≥432
  - `daemon/version` = tag ใหม่ (started ≈ เวลา push) ; `daemon/url` = tunnel ใหม่ ; `daemon/heartbeat` เดิน
  - `kernel/leader` = v175 (started ตรงกัน) ; `kernel/last-handover` มีร่องรอย v174 yield
  - ตาราง `state`/`tunnel` หาย; `system_state`/`chat_sessions`/`chat_turns`/`llm_providers` อยู่
- [ ] **Step 3: healthz** `curl -s https://<daemon/url>/healthz` Expected: 200
- [ ] **Step 4: smoke chat** ยิง completion สั้น (model ที่ใช้อยู่ เช่น Opencode free) ผ่าน tunnel → ได้ข้อความจริง; `chat_turns` count +2 (user+model); session ใหม่ใน `chat_sessions`
- [ ] **Step 5: ถ้า boot ไม่ขึ้น** — ตรวจ `kernel/bootfail` (replacement ถูกนับ/หยุดหรือยัง), `sync/apply-stage` (apply ถึงขั้นไหน), apply.log ผ่าน HF snapshot; ถ้า bounded replacement หยุดแล้ว → escalate: หยุดระบบ รอ manual + พิจารณา rollback runbook (spec §6)

### Task 18: Verify checklist + ปิดรุ่น (E6)

- [ ] **Step 1: ตรวจ D1 ชุดเต็ม** (query ตรงกับ E5 Step 2 + เพิ่ม): `llm_providers` = 6 แถว ไม่มีคอลัมน์ `free`; คีย์ใหม่ครบ (`engine/settings`,`sync/hf-token`,`sync/hf-last` หลัง push รอบแรก,`kernel/push-now`="0",`sync/migrate`,`daemon/*`,`kernel/leader`); คีย์เก่าทั้งชุดหาย (`settings`,`session/leader`,`handover/*`,`push/now`,`hf/*`,`apply/stage`,`config:*`,`diag/record-shape`,`sessions/%`); index = `idx_chat_sessions_updated`,`idx_chat_turns_session_seq` เท่านั้น (ไม่มี `idx_turns_job`)
- [ ] **Step 2: แอปบนมือถือ (user)**: discovery เจอ daemon ใหม่ + history โหลด + ส่งข้อความได้ Expected: user ยืนยัน
- [ ] **Step 3: เขียน `/tmp/cfg2-backup/switch_report.md`** — receipts (sync/migrate at/counts), tags (engine run id, dataset version, kernel version N, app run id), ผล checks, เวลาเทียบ deadline 09:35 UTC
- [ ] **Step 4: โน้ตงานค้างรอบหลัง** (ไม่ทำตอนนี้): ลบ fallback ชื่อเก่าในแอป + notebook (หลัง soak), ลบ `diag-hfsync2` session, อัปเดต AGENTS.md ของ engine (schema ใหม่)

---

**หมายเหตุลำดับ:** งาน 1-15 ทำล่วงหน้าได้ทั้งหมดแบบไม่กระทบระบบที่รัน (inert); จุดที่ระบบเปลี่ยนจริงคือ Task 17 เท่านั้น; Task 16 (ติดตั้ง APK) ควรเสร็จก่อน Task 17 เพื่อลดช่วงที่แอปเก่ามองไม่เห็น daemon

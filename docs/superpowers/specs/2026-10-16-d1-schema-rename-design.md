# D1 schema rename + ลบของตาย + ถอด `free` — design

- วันที่: 2026-10-16
- สถานะ: แผนที่ชื่ออนุมัติในแชทแล้ว (16 ต.ค. 2026) — เอกสารนี้คือรายละเอียดรออนุมัติ
- ขอบเขต: `Tulipskun/aixodia-engine` (Go daemon), `Tulipskun/AIxodia` (Android app), `kyomusen/aixodia-config` (Kaggle dataset: apply.sh/hf-sync.py), kernel notebook (push10), operator scripts
- ที่มา: "ตั้งชื่อใหม่ให้เข้าใจง่ายๆ … อันไหนซ้ำซ้อนก็ยุบรวมหรือลบ … free ใน providers ควรลบออก เราจะ fetch ทุก model แล้วกรองเอาผ่าน app"

## 1. Intent — ทำทำไม

D1 ทั้งฐานถูกอ่าน/เขียนโดยคนเดียว (มนุษย์) และแอปในมือ — ชื่อ `state`, `tunnel`, `sessions`, `turns`, `providers` และคีย์ `push/now`, `hf/last`, `handover/*` ไม่บอกหน้าที่ และมีของยุค Worker ที่ตายแล้วปนอยู่ (`config:*`) เป้าหมายคือ เปิด D1 แล้วรู้ทันทีว่าแต่ละชิ้นทำอะไร ใครเขียน ใครอ่าน และไม่มีแถวซ้ำ/ตายค้างอยู่

## 2. Goals / Non-goals / Anti-goals

**Goals**
- Tables/keys สื่อความจากชื่อ: `system_state`, `chat_sessions`, `chat_turns`, `llm_providers`; คีย์มี namespace ชัด (`daemon/*`, `engine/*`, `kernel/*`, `sync/*`)
- `tunnel` (ตารางแถวเดียว) ยุบเป็นคีย์ `daemon/*` ใน `system_state`
- ลบ `config:provider`, `config:system`, `config:entry`, `diag/record-shape`; เอา `DELETE … key='sessions/<id>'` ออกจากแอป
- ถอด `free` ออกจาก schema + engine + app ครบวงจร (การกรองฟรี/ไม่ฟรีเป็นเรื่องของแอปในอนาคต)
- `push/now` → `kernel/push-now` แบบชัดเจน `"1"`=สั่ง / `"0"`=ว่าง + กัน cascade (clear ไม่ผ่าน → ไม่ push)
- ข้อมูลแชทครบเดิมหลัง migration (ปัจจุบัน sessions=20, turns=432)

**Non-goals**
- ไม่แตะ `_cf_KV` (Cloudflare ภายใน)
- ไม่เปลี่ยน HTTP API paths/รูปแบบ (`/v1/sessions`, `/api/models`, …) ยกเว้นตัด `free_only`/`free` ออกจาก JSON
- ไม่ทำ dual-schema engine (ดู §7: ใช้ boot ordering ที่ควบคุมได้แทน)
- ไม่ยุบ `kernel/leader` กับ `daemon/ready` เป็นคีย์เดียว (เจ้าของคนละตัว เขียนคนละจังหวะ)
- ยังไม่ทำ UI กรอง "ฟรี/ไม่ฟรี" ในแอป (อนาคต ตามที่คุยกัน)

## 3. แผนที่ชื่อใหม่ (อนุมัติแล้ว)

### 3.1 Tables
| เดิม | ใหม่ | หน้าที่ |
|---|---|---|
| `state` | `system_state` | KV กลางของระบบ (leader, settings, receipts, flags) |
| `tunnel` | **ยุบเข้า `system_state`**: `daemon/url`, `daemon/version`, `daemon/heartbeat` | ตำแหน่ง tunnel + heartbeat ของ daemon ที่ serve อยู่ |
| `sessions` | `chat_sessions` | session แชท |
| `turns` | `chat_turns` | ข้อความใน session |
| `providers` | `llm_providers` | provider LLM ที่ตั้งไว้ |
| `_cf_KV` | คงเดิม | ของ Cloudflare |

### 3.2 Keys ใน `system_state`
| เดิม | ใหม่ | เขียนโดย | อ่านโดย |
|---|---|---|---|
| `settings` | `engine/settings` | engine (API) | engine |
| `session/leader` | `kernel/leader` | notebook | notebook |
| `handover/ready` | `daemon/ready` | engine (boot) | มนุษย์/operator เท่านั้น (ไม่มีโค้ดอ่าน — ตรวจแล้ว) |
| `handover/last` | `kernel/last-handover` | notebook | มนุษย์ |
| `push/now` | `kernel/push-now` | operator (มนุษย์/pipeline) | notebook |
| `hf/token` | `sync/hf-token` | operator (ครั้งเดียว) | notebook (cell0+cell1) |
| `hf/last` | `sync/hf-last` | hf-sync.py | มนุษย์ |
| `apply/stage` | `sync/apply-stage` | apply.sh | มนุษย์ |
| — (ใหม่) | `sync/migrate` | migrate.py | มนุษย์ |

### 3.3 ลบถาวร
- `config:provider` — รูปแบบ provider เก่ายุค Worker (13 วัน) — ไม่มีผู้อ่านใน engine/app/notebook/scripts (grep แล้ว); ซ้ำกับตาราง `llm_providers`
- `config:system` — ซ้ำกับ `settings` (defaults main/sub)
- `config:entry` — config ยุค mobile worker (worker ถูกปลดแล้ว ตาม `db/README.md` ของแอป)
- `diag/record-shape` — ไม่พบที่มา/ผู้อ่าน
- แอป: `DELETE FROM state WHERE key='sessions/<id>'` (vestigial — ปัจจุบัน 0 แถว; migrate จะกวาด `sessions/%` ทิ้งด้วย)
- คอลัมน์ `free` ใน `llm_providers`
- index `idx_turns_job` (ไม่มีโค้ดสร้าง/ใช้); อีก 2 ตัวเปลี่ยนชื่อ (ดู §6)
- `nodes`/`devices` — **ไม่มีอยู่ใน DB จริง** (ตรวจ `sqlite_master` แล้ว: เหลือ 6 ตาราง) — `db/schema.sql` ที่ยังพูดถึงมันคือเอกสารเก่า จะเขียนใหม่ (§8)

สำรองค่า legacy ก่อนลบ: `legacy_config_backup.json` (ในเครื่อง operator) — พร้อมขึ้น HF กับงานนี้

## 4. สภาพปัจจุบัน (หลักฐานจากโค้ด 16 ต.ค.)

| ของ | เขียนโดย | อ่านโดย |
|---|---|---|
| `providers` | engine (`api.go` 182 INSERT / 203 DELETE / 231 UPDATE) | engine (`config/config.go:28`) |
| `sessions` | engine (`session/session.go`) **+ แอปเขียนตรง** (`D1Api.kt`) | engine + **แอปอ่านตรง** |
| `turns` | engine | engine + **แอปอ่านตรง** |
| `tunnel` | engine (`db.go SaveTunnel/TouchTunnel`, ทุก 30s) | engine (`main.go:428` newerDaemonRunning), **แอป** (`D1Api.node()` → DaemonDiscovery), operator scripts |
| `state` | engine (`api.go:404`, `handover.go:66`), notebook (leader/handover/push), apply.sh, hf-sync.py | ตามนั้น |
| `_cf_KV` | Cloudflare | Cloudflare |

**`free` ทั้งสาย**: `llm_providers.free` → `config.Provider.Free` (`config/config.go:90`) → `provider.Provider.Free` (`provider/types.go:109`, set ที่ `chat.go:1062`) → `free_only` ใน `api.go` (98/154/187) + `free` ใน `gateway.go:118` (/v1/providers) → แอป: `ChatModels.kt:101`, `HistoryApi.kt:318/326`, `ProvidersScreen.kt` (badge 274, toggle 416-452), `SettingsScreen.kt` (557-560, 981-1022)
**ข้อเท็จจริงสำคัญ**: ไม่มีโค้ดตรงไหนใช้ `free` กรอง/ตัดสินใจ — เป็นป้ายโชว์ + ค่าที่เก็บ ถอดได้โดยไม่กระทบพฤติกรรม (`/v1/models` ส่งทุก model อยู่แล้ว)

D1 รองรับ `ALTER TABLE … RENAME TO` และ `DROP COLUMN` — ทดสอบสดบน D1 แล้ว (สร้าง/rename/drop ตาราง probe) ✓

## 5. Merge: `tunnel` → `daemon/*` ใน `system_state`

- ตอน start daemon: upsert `daemon/url` + `daemon/version` (เดิม `SaveTunnel` = Delete+Insert ทั้งแถว) — ลบโค้ด `CREATE TABLE tunnel`/`ALTER … ADD COLUMN` เก่าทิ้ง
- ทุก 30s: upsert `daemon/version` + `daemon/heartbeat` (เดิม `TouchTunnel` UPDATE) — คีย์ heartbeat เปลี่ยนทุก beat; version ค่าเดิม
- อ่านฝั่ง engine: `newerDaemonRunning` อ่าน version+heartbeat จากคีย์
- อ่านฝั่งแอป: query เดียว `SELECT key, value FROM system_state WHERE key IN ('daemon/url','daemon/version','daemon/heartbeat')` — fallback: ถ้าอ่านไม่ได้ (`no such table` หรือไม่พบคีย์) → อ่านตาราง `tunnel` แบบเก่า (ดู §7.2)
- ค่า format เดิมคงไว้: version = `"<tag> started=<unix>"` (แอป parse uptime จาก `started=`)

## 6. Migration design

**ที่รัน**: `apply.sh` (config dataset) เรียก `migrate.py` (ใหม่) เป็น**ขั้นแรกสุด ก่อน `note start`** — เพราะ note() รุ่นใหม่เขียน `sync/apply-stage` เข้า `system_state` ซึ่งเขียนได้หลัง rename เท่านั้น (เดิมวางไว้หลัง `note start` = ชนกันจริงตอนสลับ — reviewer จับได้) และทุกขั้นต้องจบ**ก่อน engine สตาร์ท** แต่ engine ถูกสตาร์ทโดย cell0 แยกจาก apply.sh → มี gate ที่ cell0 (ดู §7.1)

**กติกา rename คีย์**: ถ้าทั้งชื่อเก่าและชื่อใหม่มีอยู่พร้อมกัน → **แถวที่ `updated_at` ใหม่กว่าชนะ** (INSERT..SELECT + `ON CONFLICT(key) DO UPDATE` เมื่อใหม่กว่า แล้ว `DELETE` เก่า) — `migrate_revert.py` ใช้กติกาเดียวกันเป๊ะ (สมมาตร)

**Retry**: `migrate.py` ลองเองสูงสุด 3 ครั้ง (D1 สะดุดชั่วคราวได้)

ขั้นตอน (guard ตามสภาพจริงทีละข้อ, idempotent, รันซ้ำปลอดภัย):
0. rename ชื่อเก่าที่มีอยู่: `state`→`system_state`, `sessions`→`chat_sessions`, `turns`→`chat_turns`, `providers`→`llm_providers` (ข้ามถ้าชื่อเก่าไม่มี)
1. `CREATE TABLE IF NOT EXISTS` ทั้ง 4 ตาราง + index ชื่อใหม่ (DB ใหม่/บู้ตครึ่งทางได้เสมอ — หลังตัดโค้ดสร้างตารางออกจาก engine ที่นี่คือแหล่ง schema เดียว)
2. ถ้ามี `tunnel` → อ่านแถว → merge เป็นคีย์ `daemon/url|version|heartbeat` (เฉพาะค่าที่ไม่ว่าง) → `DROP TABLE tunnel`
3. `ALTER TABLE llm_providers DROP COLUMN free` (กลืน error ถ้าไม่มีคอลัมน์แล้ว)
4. rename คีย์ตามตาราง §3.2 ตามกติกา `updated_at` (เมื่อชนกัน)
5. `DELETE` คีย์: `config:provider|system|entry`, `diag/record-shape`, และกวาด `sessions/%`
6. index: `DROP INDEX IF EXISTS idx_sessions_updated|idx_turns_session_seq|idx_turns_job` → สร้าง `idx_chat_sessions_updated`, `idx_chat_turns_session_seq`
7. นับ sessions/turns ก่อน-หลัง (ให้เท่ากัน) → เขียน receipt `sync/migrate` = `{"ok":…, "sessions":…, "turns":…, "at":…}`

**Failure policy วันสลับ**: ถ้า migrate ล้ม → ไม่มี receipt `ok` → cell0 **ไม่สตาร์ท engine** → session นั้นไม่ serve → push replacement แบบ bounded (§7.1 ข้อ 3) จนผ่านหรือหยุดรอคน — ดีกว่า engine ใหม่เจอ schema เก่าแล้วตายทันที (`SaveTunnel` เป็น Fatal — reviewer ยืนยัน `main.go:104`)

**FK**: `chat_turns.session_id REFERENCES chat_sessions(id)` — rename ของ SQLite จะปรับ reference ให้เองเมื่อ `legacy_alter_table=off` (D1 ปกติ) — builder ต้องเช็กผลจริงหลัง rename (ถ้าไม่ปรับ: ไม่ระเบิด เพราะ D1 ปิด FK enforcement เป็นค่าเริ่มต้น และแอปลบ turns ก่อน sessions ด้วยมืออยู่แล้ว — ให้บันทึกผลไว้)

**Race ระหว่าง migrate**: engine v174 (โค้ดเก่า) ยังเขียน `tunnel`/`state` อยู่จนกว่า v175 จะ claim leader แล้ว v174 yield — error ชั่ววินาที–นาที (config reload fail แค่ log ไม่ตาย; แชทจะไม่ผ่านถ้าใช้จังหวะนั้น) — ยอมรับได้: เลือก push ตอนไม่มีคนใช้

**Rollback (runbook ครบวง — DB อย่างเดียวไม่พอ เพราะ `engine-latest` ถูกทับทุก push)**
0. ก่อน deploy: archive binary ปัจจุบัน (v174) ขึ้น HF work repo เผื่อกู้มือ (apply.sh ยังไม่มีกลไก pin URL)
1. `git revert` commit งานนี้บน engine → push `main` → CI republish `engine-latest` (รอ build ~2–5 นาที)
2. dataset version ใหม่: `apply.sh` เรียก `migrate_revert.py` แทน `migrate.py` (revert ใช้กติกา `updated_at` + คืน `tunnel` row จากคีย์ `daemon/*` + คืนคอลัมน์ `free` default 0 + คืน index เก่า)
3. notebook push11 = push9 (โค้ดคีย์เก่า, ไม่มี gate)
ระหว่าง rollback service ล่ม — ยอมรับได้ (ผู้ใช้คนเดียว, ใช้เวลาไม่กี่นาที/ขั้น); versioned release + pin URL = งานอนาคต (non-goal รอบนี้)

## 7. Rollout + Compatibility

### 7.1 เงื่อนไขบังคับ + ลำดับ
engine ใหม่ (อ่านชื่อใหม่) และ migrate (สร้างชื่อใหม่) ต้อง**ถึงพร้อมกันในบู้ตเดียว** — engine ใหม่+DB เก่า = พัง, DB ใหม่+engine เก่า = พัง เลือกลำดับนี้:
1. ลง engine `main` (CI rolling release อัตโนมัติ — **ยังไม่มีผลจนกว่าจะบู้ต**) — commit เฉพาะไฟล์งานนี้; อย่าแตะ `session/block.go` ที่ค้างอยู่ (งานของเจ้าของ)
2. dataset `aixodia-config` version ใหม่: `apply.sh` (+ขั้น migrate) + `migrate.py` + `hf-sync.py` (คีย์ใหม่) — ยังไม่มีผลกับ session ที่รันอยู่ (mount แช่ที่ version เดิม)
3. notebook **push10** (จุดสลับจริง v175 — ทำก่อน handover ของ v174 ~09:35 UTC 17 ต.ค.): คีย์ใหม่ทั้งชุด + `kernel/push-now` 1/0 + blob regen + **3 อย่างที่ reviewer จับได้**:
   - **Gate**: cell0 รอ receipt `sync/migrate` สด (`at` ≥ เวลา boot นี้, ok=true, timeout ~5 นาที) ก่อน download/สตาร์ท engine — กัน engine ใหม่เจอ schema เก่า (`SaveTunnel` fail = ตายทันที)
   - **cell0 แก้ชื่อ**: `d1_value_cell0` SQL `FROM state`→`FROM system_state`; อ่าน `sync/hf-token` ถ้าว่าง fallback `hf/token` (ช่วงเปลี่ยนผ่าน — ลบรอบหน้า; ไม่งั้นบู้ตสลับ HF pull จะถูกข้ามเงียบๆ เพราะอ่านตอนที่ migrate ยังไม่รัน)
   - **cell1 bounded replacement**: เส้น `if not wait_for_engine()` เดิมตายเงียบ (ไม่ push) → เพิ่ม start_replacement โดยนับในคีย์ `kernel/bootfail` `{count, first_at}` — ไม่เกิน **3 ครั้ง/30 นาที** เกินแล้วอยู่เฉย + log (กันวน push ถ้า migrate ล้มซ้ำ — reviewer ยืนยันว่า self-heal ที่ §7.1 อ้างเดิม "ไม่มีกลไกจริง" จนกว่าจะเพิ่มข้อนี้)
4. APK ใหม่ (fallback อ่านชื่อเก่าได้ §7.2) — ติดตั้งก่อนหรือหลังข้อ 3 ก็ได้; **ถ้า APK เก่าอยู่หลัง migrate**: discovery หา daemon ไม่เจอ (ตาราง tunnel หาย) จนกว่าจะติดตั้งใหม่ — ผมจะบอกจังหวะ
5. Verify: `system_state` มีคีย์ daemon/* + engine/settings + kernel/* + sync/*; ไม่มี `state`/`tunnel`; `chat_sessions`=20+ / `chat_turns`=432+; `llm_providers` 6 แถว ไม่มีคอลัมน์ free; คีย์เก่าหาย; `sync/migrate` ok; apply.log มีบรรทัด migrate; healthz ok; kernel/leader ถูก claim; แอป discovery รับ URL ใหม่ + history โหลดได้
6. เก็บกวาดรอบหลัง: ลบ fallback ในแอป, ลบไฟล์สำรอง

Race: ถ้ามีบู้ตแทรกกลางข้อ 1–2 (ไม่ควรมี — ดูเหตุผลด้านล่าง) → บู้ตนั้นเสีย 1 รอบ (engine ใหม่เจอ schema เก่าแล้วตาย; notebook เก่ายังไม่มี bounded replacement) รอบถัดไปจัดการต่อ; หลบโดยทำข้อ 1–2 ให้ติดกันในนาทีเดียว + ไม่ push kernel คั่น; ตอน deploy: v174 เพิ่งบู้ต (`kernel/push-now` ว่าง, handover อีก ~11 ชม.) → ไม่มีบู้ตเองในช่วงทำงาน + เราคุม push kernel เอง

### 7.2 Compatibility
- **แอป**: อ่านชื่อใหม่ก่อน; เจอ `no such table` → อ่านชื่อเก่า (precedent: `TURN_COLUMNS_LEGACY`) ครอบคลุม `chat_sessions`/`chat_turns`/`system_state`(node read) — ติดตั้งก่อน migrate ก็ทำงาน (ผ่านทางเก่า) หลัง migrate ก็ทำงาน (ทางใหม่)
- **JSON/API**: `free_only` ออก — แอปเก่า decode ขาด field = default false ✓; แอปใหม่ไม่ส่ง field เกิน — engine ignore ✓ (builder เช็ก decode strictness ตอนทำ)
- **Engine**: single-schema, ไม่มี tolerance layer (เหตุผล: boots ถูกควบคุมด้วยมือทั้งหมด + ลดโค้ดถาวร; ความเสี่ยงตกที่ข้อ 7.1 race — บู้ตแทรกจะเสีย 1 รอบ; bounded replacement มีเฉพาะตั้งแต่ push10)

## 8. Deliverables (ต่อไฟล์)

**engine (Tulipskun/aixodia-engine)**
- `config/config.go`: `Select("providers")`→`llm_providers`; ตัด `Free` + `boolValue` (ถ้าไม่เหลือใช้)
- `provider/types.go`: ตัด `Provider.Free` + ทุกจุด set (`io/gateway/chat.go` toInternalProvider)
- `db/db.go`: `SaveTunnel`→upsert `daemon/url|version` (+heartbeat แรก); `TouchTunnel`→upsert `daemon/version|heartbeat`; ตัดโค้ดสร้าง/ALTER ตาราง tunnel; index names ใหม่ (แนะนำ rename ฟังก์ชันเป็น SaveDaemon/TouchDaemon)
- `main.go`: `newerDaemonRunning` อ่านคีย์ใหม่
- `io/gateway/api.go`: providers SQL 3 จุด→`llm_providers`; ตัด `free_only` (98/154/187); `settingsStateKey`→`engine/settings`; `Select("state")`→`system_state`; sessions SQL→`chat_sessions`
- `io/gateway/gateway.go`: `/v1/providers` ตัด `free`
- `session/session.go`: SQL ทั้งหมด sessions→`chat_sessions`, turns→`chat_turns` (138/145/168/171/183/188/294/316/327/477)
- `handover.go`: `handover/ready`→`daemon/ready`; INSERT เข้า `system_state`
- เทสต์ทั้งหมดที่อ้างชื่อเก่า (`io/gateway/api_test.go` 92/106 ฯลฯ)
- เอกสารนี้ + commit

**app (Tulipskun/AIxodia)**
- `data/remote/D1Api.kt`: ทุก SQL ใช้ชื่อใหม่ + fallback ชื่อเก่า; ลบ `sessions/<id>` delete; `node()` อ่านคีย์ `daemon/*` + fallback ตาราง `tunnel`
- `data/model/ChatModels.kt`: ตัด `freeOnly`
- `data/remote/HistoryApi.kt`: ตัด param/field `free_only` (318/326)
- `ui/providers/ProvidersScreen.kt`: ตัด badge+toggle (274/416/442/452)
- `ui/settings/SettingsScreen.kt`: ตัด toggle+param (557-560/981-1022)
- เทสต์ที่อ้าง `freeOnly`
- `db/schema.sql`: เขียนใหม่ = DDL ชื่อใหม่ครบ (chat_sessions/chat_turns/system_state/llm_providers + index) ตัด `nodes`/ชื่อเก่า — เป็นแหล่ง provision จริงของ DB ใหม่ (engine ไม่สร้างตารางเองแล้ว)
- `db/README.md`: เขียนใหม่ให้ตรงสภาพ (ตัด `nodes`/`ai/sdk`/wrangler ให้เหลือความจริง: daemon keys ใน `system_state`, ชื่อตารางใหม่)
- `config/aixodia.example.json`: อัปเดตบล็อก `state_keys` เป็นชุดใหม่ + มาร์คเป็นเอกสารยุคเดิม

**config dataset (kyomusen/aixodia-config)**
- `apply.sh`: `apply/stage`→`sync/apply-stage`; เพิ่มเรียก `migrate.py` ก่อนโหลด engine
- `migrate.py`: ใหม่ (ขั้น §6 + กติกา `updated_at` + retry 3 + create-if-missing) + receipt `sync/migrate`
- `migrate_revert.py`: ใหม่ (rollback ตาม runbook §6)
- `hf-sync.py`: `hf/last`→`sync/hf-last`

**notebook (operator, /tmp/nbtest → push10)**
- cell0: `d1_value_cell0` SQL→`system_state`; `hf/token`→`sync/hf-token` (+fallback `hf/token` ชั่วคราว); **gate** รอ `sync/migrate` ก่อนสตาร์ท engine
- cell1: `LEADER_KEY`→`kernel/leader`; `hf/token` (fallback เหมือน cell0); `handover/last`→`kernel/last-handover`; push-now→`kernel/push-now` + "1"/"0" + clear-verify + ไม่ push ถ้า clear ไม่ผ่าน; **bounded replacement** (`kernel/bootfail`, 3/30 นาที) บนเส้น engine-not-up
- regen `NOTEBOOK_B64` (สูตรเดิมจากรอบ v171) + push

**operator scripts**: ตรวจแล้วตัวจริงมีสองที่ — (1) `/tmp/nbtest` (`wsc.py`, `watch*.sh` และ monitor ad hoc — อัปเดตคีย์ที่อ่านเป็น `daemon/*`, `kernel/leader`, `sync/hf-last`) (2) `/root/Aixodia/kaggle/README.md` = คู่มือ operator (แก้ `session/leader`/`push/now`/`handover/last` เป็นชื่อใหม่) — ไม่มี operator script อื่นแตะ D1 (`aix-save.sh`/`aix-restore.sh` เป็น file-level)

## 9. Left to builder (ตัดสินเองได้)

- รูปแบบโค้ด/เทสต์, ชื่อฟังก์ชันภายใน (แนะนำ SaveDaemon/TouchDaemon)
- `migrate.py`: ยิง REST ทีละคำสั่ง + print สั้น (batch ได้ถ้า D1 endpoint รับ)
- ขั้นตอน regen blob ของ notebook (ใช้สูตรเดิม)
- APK build/sign/version ตามเดิม
- เช็กผล FK reference หลัง rename → บันทึก (ไม่ต้องแก้ ถ้าไม่ auto)
- อัปเดตเทสต์ทุกที่อ้างชื่อเก่า

## 10. Calls ของผู้เขียนเอกสาร (ขอให้รีวิว)

1. `daemon/beat` → `daemon/heartbeat` (ชัดกว่า)
2. `kernel/push-now`: `"1"`=สั่ง, `"0"`=ว่าง; clear→อ่านยืนยัน→ไม่ผ่านให้ retry 1 ครั้ง→ยังไม่ผ่าน **ไม่ push** (แก้ราก cascade ของ v171)
3. แยก `migrate.py`/`migrate_revert.py` เป็นไฟล์ใน dataset + receipt `sync/migrate`
4. ทิ้ง `idx_turns_job`; สร้าง index ใหม่ชื่อใหม่ 2 ตัว
5. Fallback ของแอป = เฉพาะ "no such table" (ไม่ทำ dual-key ยิบย่อย)
6. `db/README.md` + `aixodia.example.json` อัปเดตตาม §8
7. ฟังก์ชันภายใน engine: `SaveDaemon`/`TouchDaemon`
8. **Gate วันสลับ**: engine ไม่สตาร์ทจนกว่า `sync/migrate` ok (timeout ~5 นาที); migrate ล้ม → session ไม่ serve → push replacement bounded **3 ครั้ง/30 นาที** แล้วหยุดรอคน — บู้ตแบบนั้นเสียเปล่าแต่ไม่เสียหาย
9. **กติกา rename คีย์ทั้งขาไป-กลับ**: `updated_at` ใหม่ชนะ
10. **Rollback runbook 3 ขั้น** (§6) + archive binary v174 ขึ้น HF ก่อน deploy — ยอมรับ downtime สั้น; versioned pinning = อนาคต
11. **`db/schema.sql` เขียนใหม่** เป็นแหล่ง provision จริง + migrate มี create-if-missing สำรอง
12. **notebook fallback ชั่วคราว**: `sync/hf-token` → ถ้าว่างอ่าน `hf/token` (ลบรอบหน้า)

## 11. push/now — พฤติกรรมเดิม (อ้างอิง)

เดิม (push9): `d1_value("push/now").strip()` ไม่ว่าง = สั่ง; notebook clear เป็น `""` **ก่อน** push; ถ้า clear พลาด → `"pushing anyway"` → flag ค้าง → session ถัดไป push อีก = cascade (เกิดจริงกับ v171). ไม่ใช่ 0/1 — เป็น empty/non-empty

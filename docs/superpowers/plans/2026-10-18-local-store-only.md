# Local store only: D1 เหลือ daemon discovery — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ย้ายทุกอย่างที่ engine เก็บลง local SQLite (provider + api key, settings, agent pin ต่อห้อง, handover note) เหลือ D1 แค่ `daemon/url|version|heartbeat`

**Architecture:** แพ็กเกจใหม่ `localdb` เป็นเจ้าของ path + schema ของ local DB ไฟล์เดียว เปิด `*sql.DB` ครั้งเดียวแล้วแจก handle ให้ `session`, `config` และ gateway; ลบ D1 session store / `importd1.go` / index creation บนตารางแชท; import provider จาก D1 ครั้งเดียวด้วย gate "local ว่าง"

**Tech Stack:** Go 1.22 (stdlib only), `modernc.org/sqlite`, D1 REST ผ่าน `aixodia-engine/db`

**Spec:** `docs/superpowers/specs/2026-10-18-local-store-only-design.md`

## Global Constraints

- แก้เฉพาะ repo `Tulipskun/aixodia-engine` — ห้ามแตะ `aixodia-app` (ผู้ใช้แก้เอง)
- HTTP API path/รูปแบบห้ามเปลี่ยน (`/api/providers`, `/api/providers/{id}/keys`, `/api/settings`, `/api/sessions/{id}`, `/v1/*`)
- D1 ที่ engine เขียนได้เหลือ 3 คีย์: `daemon/url`, `daemon/version`, `daemon/heartbeat`
- local DB ไฟล์เดียว (`AIXODIA_DB_PATH`, default `/kaggle/working/aixodia.db`, local dev `./aixodia.db`); log DB แยกไฟล์เหมือนเดิม ห้ามแตะ `d1log.go`
- ทุก task ต้องผ่าน: `go build ./... && go vet ./... && gofmt -l . && go test ./...`
- ข้อความ log/error เป็นภาษาอังกฤษ, comment อธิบาย "ทำไม" ไม่ใช่ "อะไร"

## Rollback

- ก่อน Task 7 (ยังไม่ drop): `git revert` แล้ว republish `engine-latest` ก็กลับได้ (local DB ยังอยู่ครบใน snapshot)
- หลัง Task 7: เป็นทางเดียว (ตาราง D1 หาย) — กู้ได้แค่จาก dump `/root/d1-dump-2026-10-18.json` + snapshot ล่าสุดบน HF

## Review Focus

1. provider เพิ่ม/ลบ/เปลี่ยนคีย์ แล้ว session ใหม่ (restart) → ต้องคงอยู่ (เพราะอยู่ใน snapshot)
2. local providers ว่าง + รอบนั้น D1 อ่านไม่ผ่าน → ต้องได้ลองใหม่ ไม่ใช่ provider หายทั้งชุด
3. ปักหมุด `sub_*` รายห้อง ต้องไม่ถูกเขียนทับด้วยค่าว่างเมื่อแชทสำเร็จ
4. settings ค่าเก่าไม่ถูกย้าย → แอพต้องยังทำงานด้วย default ได้ (ไม่ error)
5. `/api/sessions/{id}` ของ session ที่ไม่มี → 404 เหมือนเดิม ไม่ใช่ 500

---

### Task 1: `localdb` — path + schema + handle เดียว

**Files:**
- Create: `localdb/localdb.go`, `localdb/localdb_test.go`
- Modify: `session/sqlite.go:16-95` (เปิด DB + migrate ย้ายออก), `session/sqlite_test.go`, `main.go:60-71`, `main.go:478-499`

**Interfaces:**
- Produces: `localdb.Path() string`, `localdb.Open(path string) (*sql.DB, error)` — สร้างไฟล์+โฟลเดอร์, DSN ต่อ `?_pragma=busy_timeout(5000)`, `SetMaxOpenConns(1)`, migrate ตาราง `chat_sessions`, `chat_turns`, `llm_providers(idx, provider, endpoint, keys, adapter, free)`, `state(key, value)`
- Produces: `session.NewSQLite(db *sql.DB) (*SQLite, error)` (รับ handle ไม่เปิดเอง)
- Consumes: DDL เดิมใน `session/sqlite.go:47-98` ย้ายมาตรง ๆ

- [ ] **Step 1: เขียนเทสต์ที่ยังไม่ผ่าน** (`localdb/localdb_test.go`)

```go
func TestOpenCreatesEveryLocalTable(t *testing.T) // Open(temp) แล้ว sqlite_master ต้องมี chat_sessions, chat_turns, llm_providers, state
func TestPathPrefersEnv(t *testing.T)             // AIXODIA_DB_PATH wins; ค่าว่าง → ./aixodia.db (ไม่มี /kaggle/working บนเครื่องเทสต์)
func TestSessionStoreUsesGivenHandle(t *testing.T) // localdb.Open(temp) → session.NewSQLite(db) → Create/Get ได้
```

- [ ] **Step 2: รันให้เห็นว่าล้ม** — `go test ./localdb/` → `no required module provides package aixodia-engine/localdb`
- [ ] **Step 3: implement `localdb`** — `Path()` (env → `/kaggle/working/aixodia.db` ถ้ามี dir → `./aixodia.db`), `Open()` ตาม Interfaces; ย้าย DDL ของ chat_* จาก `session/sqlite.go` มาไว้ที่นี่ (รวม `ALTER TABLE ... ADD COLUMN` แบบ ignore duplicate)
- [ ] **Step 4: `session.NewSQLite(db *sql.DB)`** — ตัดการเปิด/ migrate ออก; แก้ `session/sqlite_test.go` ให้ใช้ `localdb.Open(t.TempDir()+"/a.db")`
- [ ] **Step 5: `main.go` ใช้ local store อย่างเดียว** — `openSessionStore()` เหลือ `localdb.Open(localdb.Path())` + `session.NewSQLite(db)`; ลบสาขา D1, `AIXODIA_SESSION_STORE`, `session.EnsureTurns()`
- [ ] **Step 6: รัน** — `go build ./... && go vet ./... && gofmt -l . && go test ./...` → ผ่านหมด; commit `localdb: own the local store path, schema and handle`

---

### Task 2: providers → local + import ครั้งเดียวจาก D1

**Files:**
- Create: `config/store.go`, `config/store_test.go`, `config/import_d1.go`
- Modify: `config/config.go:24-46` (`LoadConfig` อ่าน local), `io/gateway/api.go:180-231` (เขียน local), `main.go:73` (ลำดับ import)

**Interfaces:**
- Consumes: `localdb.Open` (Task 1), `db.Query` (D1 REST) สำหรับ import
- Produces: `config.NewStore(db *sql.DB) *Store`; `(*Store) List() ([]Provider, error)`, `Add(Provider) error`, `Remove(name string) error`, `SetKeys(name string, keys []string) error`, `Count() (int, error)`
- Produces: `config.ImportLegacyProviders(store *Store, d1 func(string, ...any) ([]map[string]any, error)) (int, error)` — อ่าน `llm_providers` จาก D1, เขียนลง local, คืนจำนวนที่ย้าย (0 ถ้า local ไม่ว่าง หรือตาราง D1 ไม่มีแล้ว)
- `Provider.ID` ยังมาจากคอลัมน์ `idx` (`SELECT idx, provider, endpoint, keys, adapter FROM llm_providers`) เพื่อเลี่ยง keyword `index`; `LoadConfig` ใช้ `providerFromRow` เดิมโดย map `idx`→`index`

- [ ] **Step 1: เขียนเทสต์ที่ยังไม่ผ่าน** (`config/store_test.go`)

```go
func TestStoreRoundTrip(t *testing.T)      // Add → List เห็น 1, ID/Name/APIURL/Adapter/Keys ครบ; Remove → ว่าง
func TestStoreSetKeys(t *testing.T)        // SetKeys([]string{"k1","k2"}) → List เห็นคีย์ใหม่เรียงเดิม
func TestImportSkipsWhenLocalNotEmpty(t *testing.T) // local มี 1 แล้ว → import คืน 0 ไม่ทับ
func TestImportIntoEmptyStore(t *testing.T)         // fake D1 rows → local เห็นครบ + api key ครบ
func TestLoadConfigReadsLocalOnly(t *testing.T)     // Set store แล้ว LoadConfig → config.GetProviders() ได้ค่าจาก local
```

- [ ] **Step 2: รันให้เห็นว่าล้ม** — `go test ./config/` → `undefined: NewStore`
- [ ] **Step 3: implement `config/store.go`** — DDL `llm_providers` อยู่ใน `localdb` แล้ว (Task 1); DSN/คำสั่ง CRUD ตรง ๆ ต่อ handle ที่รับมา
- [ ] **Step 4: implement `config/import_d1.go`** — gate: `store.Count() == 0` และ D1 read ต้องไม่ error; `no such table` → คืน 0 เงียบ ๆ; log `config: imported N providers from D1`
- [ ] **Step 5: เปลี่ยน `LoadConfig`** ให้อ่านจาก store แทน `db.Select("llm_providers")`; เพิ่ม `config.SetStore(store)` เรียกจาก `main.go` หลัง `localdb.Open`
- [ ] **Step 6: เปลี่ยน 3 handler ใน `io/gateway/api.go`** (`apiAddProvider`/`apiRemoveProvider`/`apiChangeKeys`) จาก `db.Insert/Delete/Update("llm_providers", ...)` เป็น store methods; error → 502 `database_error` เหมือนเดิม
- [ ] **Step 7: เรียก import ตอนบูต + ตอน reload** — ใน `startConfigReloader` ถ้า `store.Count() == 0` ให้ลอง `ImportLegacyProviders` ก่อน `LoadConfig`
- [ ] **Step 8: รัน** — ชุดคำสั่งเต็ม → ผ่าน; commit `providers: store in the local SQLite, import the keys from D1 once`

---

### Task 3: settings + handover note → local `state`

**Files:**
- Create: `localdb/state.go`, `localdb/state_test.go`
- Modify: `io/gateway/api.go:346-408`, `handover.go:45-84`

**Interfaces:**
- Produces: `localdb.GetState(db *sql.DB, key string) (string, bool, error)`, `localdb.PutState(db *sql.DB, key, value string) error`, `localdb.DelState(db *sql.DB, key string) error`
- Consumes: ตาราง `state(key, value)` จาก Task 1

- [ ] **Step 1: เขียนเทสต์ที่ยังไม่ผ่าน** (`localdb/state_test.go`): `PutState` แล้ว `GetState` ได้ค่า; `GetState` ของคีย์ที่ไม่มี → `found=false`, ไม่ error; `PutState` ซ้ำ → ทับค่าเดิม
- [ ] **Step 2: รันให้เห็นว่าล้ม** — `go test ./localdb/` → `undefined: GetState`
- [ ] **Step 3: implement `localdb/state.go`** — UPSERT (`ON CONFLICT(key) DO UPDATE`)
- [ ] **Step 4: ย้าย settings handlers** — `savedMainModel`/`apiGetSettings`/`apiPutSettings` ใช้ `GetState/PutState` กับคีย์เดิม `engine/settings`; ค่าที่อ่านได้ไม่ใช่ JSON ที่ถูกต้อง → คืน `defaultSettings()` เหมือนเดิม; เพิ่มเทสต์ใน `io/gateway` ว่า GET คืน default แล้ว PUT/GET ได้ค่าที่เขียน (ใช้ store เทสต์เดิมของแพ็กเกจ)
- [ ] **Step 5: ย้าย handover note** — `publishHandover` เขียน `localdb.PutState(db, "daemon/ready", payload)` ผ่าน handle ของ local store; ลบ fallback `session.NewD1()` และการอ้าง `db.Query` ในไฟล์นี้; เพิ่มเทสต์: เรียก `publishHandover` แล้ว `GetState("daemon/ready")` มี JSON ที่มี `kernel_started`
- [ ] **Step 6: รัน** — ชุดคำสั่งเต็ม → ผ่าน; commit `settings+handover: keep the engine's own notes in the local store`

---

### Task 4: agent pin รายห้อง (GET/PATCH `/api/sessions/{id}`) → session store

**Files:**
- Modify: `session/sqlite.go` (เพิ่ม method), `io/gateway/api.go:410-488`, `io/gateway/gateway.go:36-46` (interface)

**Interfaces:**
- Produces: `(*session.SQLite) SaveAgent(id, provider, model, subProvider, subModel string, subEnabled bool) error` — UPDATE เฉพาะคอลัมน์ `provider, model, sub_provider, sub_model, sub_enabled, updated_at`; ไม่แตะ title
- Produces: `SessionStore.SaveAgent(...)` ใน interface ของ gateway (mock ในเทสต์ต้องเพิ่มด้วย)
- `apiGetSessionAgent` ใช้ `sessions.Get(id)`; `apiPatchSessionAgent` ใช้ `applySessionAgentPatch` เดิม (แปลง body → ค่า) + `SaveAgent` เฉพาะเมื่อมีค่าที่จะเขียน

- [ ] **Step 1: เขียนเทสต์ที่ยังไม่ผ่าน** — `session/sqlite_test.go`: `TestSaveAgentKeepsTitleAndOtherColumns` (สร้าง session → SaveAgent → Get: provider/model/sub_* เปลี่ยน, title/created_at เท่าเดิม); `io/gateway` : `TestPatchSessionAgent` (PATCH `{"sub_provider":"X","sub_model":"Y","sub_enabled":true}` → GET คืนค่านั้น; PATCH session ที่ไม่มี → 404)
- [ ] **Step 2: รันให้เห็นว่าล้ม** — `go test ./session/ ./io/gateway/` → `undefined: SaveAgent`
- [ ] **Step 3: implement `SaveAgent`** (ระวัง: ห้ามแก้ `SaveConfig` ที่ path แชทใช้อยู่ — มันเขียน provider/model/title เท่านั้น)
- [ ] **Step 4: เปลี่ยน 2 handler** ให้ใช้ store; error จาก store → 502 `database_error`; ไม่พบ session → 404 `not_found`
- [ ] **Step 5: รัน** — ชุดคำสั่งเต็ม → ผ่าน; commit `gateway: session agent pins live in the local store`

---

### Task 5: เก็บกวาดโค้ด D1 ที่ตายแล้ว

**Files:**
- Delete: `session/session.go`, `importd1.go` (+ เทสต์ที่อ้าง)
- Modify: `db/db.go:136-144`, `main.go` (ลบ `importLegacyHistory` call), `io/gateway/gateway_test.go:483`

**Interfaces:**
- Consumes: ทุกอย่างจาก Task 1–4
- Produces: engine ที่ `grep -n "db\.\(Query\|Select\|Insert\|Update\|Delete\)" --include=*.go .` เหลือแค่ `main.go` (daemon keys) + `config/import_d1.go` (อ่านครั้งเดียว) + `db/db.go` (daemon keys)

- [ ] **Step 1: ลบไฟล์/บรรทัด** — `session/session.go` (D1 store + `EnsureTurns` + `truncate` ที่ไม่มีใครใช้), `importd1.go`, `db/db.go`'s `CREATE INDEX` บน `chat_*`, call `importLegacyHistory` ใน `main.go`
- [ ] **Step 2: แก้เทสต์ที่ใช้ `session.NewD1()`** (`io/gateway/gateway_test.go:483`) → ใช้ local SQLite ชั่วคราว (`localdb.Open(t.TempDir()+"/a.db")`)
- [ ] **Step 3: รัน** — `go build ./... && go vet ./... && gofmt -l . && go test ./...` → ผ่าน; ยืนยันด้วย grep ตาม Interfaces
- [ ] **Step 4: commit** `cleanup: drop the D1 session store, the chat import and the chat indexes`

---

### Task 6: release + deploy + verify บนเครื่องจริง (ops, ยืนยันก่อน restart)

**Files:** ไม่มี (release เดิมของ repo: `engine-latest`)

- [ ] **Step 1:** push main → รอ CI/Release สร้าง asset `engine-latest` สำเร็จ (ดู workflow ของ repo)
- [ ] **Step 2:** `POST /restart` ที่ tunnel ปัจจุบัน (ยืนยันกับผู้ใช้ก่อน — restart ทั้ง session ~1 นาที)
- [ ] **Step 3:** หลังบูต ตรวจ: `/healthz` ขึ้น, `/v1/sessions` = 25, **ไม่มี** log `import:` อีก
- [ ] **Step 4:** ตรวจ provider: `/api/providers` ต้องได้ 6 รายการจาก local; `/api/models?provider=Opencode` ตอบได้ (พิสูจน์ว่า api key ถูกย้ายจริง)
- [ ] **Step 5:** ตรวจ session agent: `GET /api/sessions/{id}` คืน provider/model ที่ปักไว้ของห้องเดิมจาก local (ไม่ใช่ D1)
- [ ] **Step 6:** ตรวจ settings: `PUT /api/settings` ด้วยค่าเดิม แล้ว `GET` ได้ค่านั้นกลับ (ยืนยันว่าเขียน local)
- [ ] **Step 7:** กด backup หนึ่งรอบ (timer หรือ `POST /restart` อีกครั้ง) → `kyomu53n/kgat-snapshot-*` ต้องมี `/kaggle/working/aixodia.db` ที่มี provider อยู่ในไฟล์ (ดึง manifest มาเช็ค)

### Task 7: D1 สะอาด + ปิด notebook เก่า (ops, ยืนยันก่อนลบ)

- [ ] **Step 1:** dump `chat_sessions`, `chat_turns`, `llm_providers`, `system_state`, `engine_logs` เป็นไฟล์เก็บนอก repo (`/root/d1-dump-2026-10-18.json`, `chmod 600` เพราะมี api key)
- [ ] **Step 2:** ยืนยัน Task 6 ผ่านครบก่อน → `DROP TABLE` ทีละตาราง (`chat_sessions`, `chat_turns`, `llm_providers`, `engine_logs`); `DELETE FROM system_state WHERE key NOT IN ('daemon/url','daemon/version','daemon/heartbeat')`
- [ ] **Step 3:** ลบ/ปิด notebook เก่า `kyomusen/aixodia` (ตัวรัน `apply.sh`→`migrate.py` ที่สร้างตารางกลับ) — dataset `kyomusen/aixodia-config` ปล่อยไว้ ไม่มีใครรัน
- [ ] **Step 4:** verify: `sqlite_master` ของ D1 เหลือ `system_state` + `_cf_KV`; แอพยัง discover + online; เปิดแชทเก่าได้

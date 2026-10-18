# เก็บทุกอย่างลง local SQLite — D1 เหลือแค่ daemon discovery — design

- วันที่: 2026-10-18
- สถานะ: รออนุมัติ (ผู้ใช้สั่งในแชท 18 ต.ค. 2026: "รื้อทั้งหมด เขียนลง local sql ล้วน ยกเว้น tunnel url อย่างเดียว" และ "ไม่มีอะไรต้องย้าย ยกเว้น providers ที่เก็บ api key")
- ขอบเขต: **`Tulipskun/ai-engine` เท่านั้น** (ผู้ใช้สั่ง "แก้ไขฝั่ง engine อย่างเดียวก่อน ฉันจะแก้ฝั่งแอพเอง") + D1 database `aixodia` + notebook เก่า `kyomusen/aixodia` เป็นงานops ที่ผมทำหลัง engine ผ่าน (ยืนยันก่อนลบ)
- ฝั่งแอพไม่แตะเลย: `aixodia-app/db/schema.sql` และ `db/README.md` ยังสร้าง/อธิบาย `llm_providers`+`system_state` อยู่ — ผู้ใช้จัดการเอง (รวม comment เก่าที่ผิดใน `HistoryApi.kt:38`)
- ที่มา: หลังย้ายประวัติแชทลง local SQLite สำเร็จ (engine v1.137 รันจริง + snapshot HF ครบวง: backup → สร้าง repo ใหม่ → push ตัวเอง → restore ไม่ import ซ้ำ) เหลือ D1 อีก 4 ส่วนที่ยังถูกใช้จริง

## 1. Intent

D1 ควรเป็น "สมุดหน้าเดียว" ที่บอกแค่ว่าเครื่องนี้อยู่ที่ URL ไหน และยังหายใจอยู่ไหม — ข้อมูลอื่นทั้งหมด (แชท, provider+api key, settings, note) เก็บใน local SQLite ไฟล์เดียวที่ถูก snapshot ขึ้น HF อยู่แล้ว

## 2. Goals / Non-goals / Anti-goals

**Goals**
- หลังงานนี้ grep ใน engine เหลือ `db.*` เฉพาะ 3 คีย์ daemon
- provider + api key ทั้ง 6 รายการย้ายมา local ครบ (ยืนยันด้วยการ refresh provider จากในแอพ / `/api/models?provider=`)
- ลบโค้ด D1 session store ทั้งไฟล์ + `importd1.go` (ไม่ต้อง import ประวัติ เพราะ local มีครบและ snapshot พิสูจน์แล้ว)
- ลบตาราง D1: `chat_sessions`, `chat_turns`, `llm_providers`, `engine_logs`; `system_state` เหลือ `daemon/url|version|heartbeat`
- แอพไม่แก้อะไร: discovery อ่าน 3 คีย์ daemon เหมือนเดิม, provider/settings/session ผ่าน HTTP API เหมือนเดิม

**Non-goals**
- ไม่เปลี่ยน HTTP API (path/รูปแบบ) ของ engine
- ไม่แตะ `_cf_KV`
- ไม่ทำ dual-mode local+D1 — ตัด D1 store ออกเลย
- ไม่แตะ snapshot/HF (kernel) และไม่แตะ `db.Verify` / การ publish tunnel

**Anti-goals**
- แอพขึ้น offline ทั้งที่เครื่องยังรัน (heartbeat/url หาย)
- api key provider หาย (ลบ `llm_providers` ก่อน import สำเร็จ)
- ลบตารางก่อน engine ใหม่ถูก deploy → สลับโมเดลรายห้องพัง

## 3. ของจริงวันนี้ (หลักฐาน file:line)

| จุด | ทำอะไร | ปลายทาง |
|---|---|---|
| `config/config.go:27` | SELECT `llm_providers` (โหลดทุก 60 วิ) | อ่าน local |
| `io/gateway/api.go:180,200,228` | INSERT/DELETE/UPDATE `llm_providers` (เพิ่ม/ลบ/เปลี่ยนคีย์) | เขียน local |
| `io/gateway/api.go:351,372,400` | `system_state['engine/settings']` (GET/PUT `/api/settings`) | local KV |
| `io/gateway/api.go:411,482` | `chat_sessions` (GET/PATCH `/api/sessions/{id}` — ปักหมุด provider/model/sub_* รายห้อง) | local store (คอลัมน์เดียวกัน) |
| `handover.go:69` | note ลง `system_state['daemon/ready']` | local KV |
| `session/session.go` + `main.go:480-499` (`AIXODIA_SESSION_STORE`) | D1 session store + `EnsureTurns()` | ลบ |
| `importd1.go` | one-shot import ประวัติ D1 → local | ลบ (local ครบแล้ว) |
| `db/db.go:138-139` | CREATE INDEX บน chat_* ทุก daemon start | ลบ |
| `main.go:42`, `db.SaveTunnel/TouchDaemon` | `daemon/url|version|heartbeat` | คงเดิม (D1) |
| ฝั่งแอพ | `D1Api.discover()` + `D1Api.node()` อ่าน 3 คีย์ daemon → `NodeInfo{tunnel_url, version, online(heartbeat < 90s)}` | คงเดิม; comment เก่า `HistoryApi.kt:38` ผิด แก้ |

## 4. รูปร่างใหม่

### 4.1 local
- ไฟล์เดียว: `AIXODIA_DB_PATH` (default `/kaggle/working/aixodia.db` — อยู่ใน snapshot แล้ว)
- ตาราง: `chat_sessions`, `chat_turns` (มีอยู่) + `llm_providers` (คอลัมน์เดิม: index, provider, endpoint, keys, adapter, free) + `state(key, value)` สำหรับ settings และ handover note
- แพ็กเกจใหม่ `localdb` เป็นเจ้าของ path + schema เปิด `*sql.DB` ครั้งเดียว แล้วแจก handle ให้ `session`, `config`, gateway
- "ไฟล์เดียว" หมายถึง data store — **log ยังเป็นไฟล์แยก** (`AIXODIA_LOG_DB_PATH` → `/kaggle/working/aixodia-logs.db`, มี rotation ของตัวเองอยู่แล้ว) ไม่ refactor `d1log.go`

### 4.2 D1
- `system_state`: `daemon/url`, `daemon/version`, `daemon/heartbeat` เท่านั้น — ไม่มีตารางอื่น

### 4.3 migration
- import provider จาก `llm_providers` **เฉพาะเมื่อ local `llm_providers` ว่าง** (ไม่ทับของใหม่) และลองซ้ำได้: ทุกครั้งที่ engine start + ตอน config reload ถ้ายังว่าง (กันรอบที่ D1 อ่านไม่ผ่านแล้ว provider หายทั้งชุด) — api key คือสิ่งที่ต้องรอด
- หลัง `DROP TABLE llm_providers` การ import จะไม่มีทางเกิดอีก (ตารางไม่มี) จึงไม่มีปัญหา provider ที่ผู้ใช้ลบไปแล้วถูก "ฟื้น"
- settings ไม่ย้ายค่าเก่า (แอพตั้งใหม่ได้)
- ประวัติแชทไม่ต้องย้าย (local มีครบและ snapshot กู้ได้แล้ว)

## 5. ลำดับปฏิบัติ

1. engine: `localdb` + ย้าย 6 จุด (§3) + ลบ D1 store/importd1/index + ตัด fallback `session.NewD1()` ใน handover
2. test: `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...` + เทสต์ใหม่ (provider CRUD, settings, agent patch บน local)
3. release rolling `engine-latest` → session ใหม่ติดตั้งเอง → verify: `/api/providers` = 6, refresh provider ผ่าน (คีย์ใช้ได้), แชท + สลับโมเดลรายห้องได้
4. สำรอง D1 (dump 4 ตารางเป็นไฟล์ local) → DROP `chat_sessions`/`chat_turns`/`llm_providers`/`engine_logs` → `system_state` เหลือ 3 คีย์ daemon
5. ปิด/ลบ notebook เก่า `kyomusen/aixodia` (ตัวที่ `apply.sh`→`migrate.py` สร้างตารางกลับ)
6. verify ปลายทาง: discovery/online, ประวัติครบหลัง restart (snapshot), D1 ไม่มีข้อมูลแชท/provider อีก

## 6. Calls ของผม (ให้ตรวจ)

- เก็บ 3 คีย์ daemon ไม่ใช่แค่ `daemon/url`: แอพใช้ `daemon/heartbeat` ตัดสิน online และ `daemon/version` แสดงผล (`D1Api.node()` → `NodeInfo`)
- settings ไม่ย้ายค่าเก่า — ถ้าต้องการย้ายด้วย เพิ่มบรรทัดเดียว
- handover note ย้ายมา local KV คีย์เดิม (`daemon/ready`) และยังอ่านได้จาก log ตอนบูตผ่าน `GET /api/logs` — ไม่เพิ่ม endpoint ใหม่
- local = ไฟล์เดียว (ไม่แยก DB ต่อโดเมน)

## 7. ความเสี่ยง / ค้าง

- `CF_TOKEN` ผิด → engine ยังตายเหมือนเดิม (ไม่แตะ)
- D1 ล่ม → แชทในเครื่องยังทำงาน แต่แอพหา tunnel ไม่เจอ (เหมือนเดิม)
- แอพเวอร์ชันเก่าที่อ่านประวัติจาก D1 จะพังหลัง drop — ผู้ใช้ยืนยัน "รื้อทั้งหมด" แล้ว

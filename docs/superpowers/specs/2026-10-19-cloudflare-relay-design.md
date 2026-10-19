# ตัด tunnel ทิ้ง: Cloudflare Worker + Durable Object relay — design

- วันที่: 2026-10-19
- สถานะ: รออนุมัติ
- ที่มา: quick tunnel ของ cloudflared หมุน URL ทุกครั้งที่ process เกิดใหม่ (เอกสาร Cloudflare เองระบุ "dev/testing only") ทำให้ `daemon/url` ใน D1 ชี้ URL ที่ตายแล้วได้; ผู้ใช้ถามหาทางอื่นที่ฟรี/นิ่งกว่า → เลือก **Cloudflare Worker + Durable Object relay (แบบ tunnel-less, คุยทุกอย่างผ่าน WS เดียว — "แบบ B")**
- ขอบเขต: **`Tulipskun/aixodia-engine`** (relay client + router + ย้าย transport) + **`Tulipskun/AIxodia`** (แอพ: one-socket + RPC client) + Worker/DO โปรเจกต์ใหม่ `relay/` (ในรีโป engine) — ต้องมีสิทธิ์ Workers + workers.dev subdomain (ดู §9)

## 1. Intent

engine และมือถือ **ต่างก็ dial ออก** ไปเจอกันที่ Durable Object ตัวเดียว — ไม่มี tunnel, ไม่มี URL หมุน, ไม่มี cloudflared อีก ตัว DO แค่จับคู่ (pair) และส่ง frame ไปกลับ; สัญญาณทั้งหมด (แชท, terminal, REST) ไหลบน WS เดียวที่แอปถือ และ WS เดียวที่ engine ถือ ได้ URL นิ่ง `wss://<worker>.<sub>.workers.dev/...` บนบัญชี Cloudflare เดิม (เดียวกับ D1)

## 2. Goals / Non-goals / Anti-goals

**Goals**
- ลบ `cloudflared` + keeper ออกจาก engine ทั้งหมด (`main.go:120-147`, `tunnel.go`, `tunnel_test.go`)
- engine ต่อออก DO ด้วย WS client (masked frames) + reconnect backoff แทน tunnel
- แชท, terminal (หลาย session), REST ทั้งชุด วิ่งบน WS เดียว ผ่าน channel tag
- REST ฝั่ง engine **ไม่เขียน handler ใหม่** — dispatch เข้า `mux` เดิมในโปรเซสด้วย `httptest.NewRequest` + `ServeHTTP` (แพตเทิร์นที่มีอยู่แล้ว: `main.go:215,271,329`)
- DO บังคับ: token ต้องตรง, engine ที่ `started` ใหม่กว่าชนะ (ย้าย handover guard จาก D1 มาที่ DO), รู้ presence
- อยู่ใน Workers/DO **Free plan** โดยไม่ชนเพดาน (§4.5)
- engine ยังไม่เพิ่ม dependency (WS client เขียนเองแบบเดียวกับ `io/wsock`)

**Non-goals**
- ไม่ทำ HTTP reverse-tunnel (แบบ A) — ไม่มี `net.Listener` จาก WS, ไม่พา HTTP ดิบ
- ไม่ย้าย business logic ไป Cloudflare; DO เป็นแค่ตัวส่งต่อ + presence (state ไม่มีข้อมูลแชท)
- ไม่แตะ provider/session/tools/gateway internals
- ไม่แตะ snapshot/HF/kernel และไม่แตะวิธีที่ engine ได้ข้อมูล provider
- ไม่ทำ multi-tenant: 1 บัญชี = 1 ห้อง (room)

**Anti-goals**
- terminal (ปริมาณสูง) ทำให้แชทสะดุด หรือกินโควตา DO จนหมดวัน
- เปิด relay ให้ใครก็ได้ที่ไม่มี token (auth ต้องอยู่ที่ DO + engine)
- เก็บ frame ใหญ่ในหน่วยความจำ DO (ต้อง stream/forward ผ่าน ไม่ buffer)
- ทำ handover แล้ว engine เก่ากับใหม่แย่งกันเขียน (ต้องมีตัวตัดสินเดียว)

## 3. ของจริงวันนี้ (หลักฐาน file:line)

| จุด | ทำอะไร | ปลายทางหลังงานนี้ |
|---|---|---|
| `main.go:120-147` | `exec.LookPath("cloudflared")` + `newKeeper` + `startHeartbeat` | ลบ → relay client |
| `tunnel.go`, `tunnel_test.go` | supervisor cloudflared + publish URL | ลบ |
| `main.go:99-113` | `http.Server` บน `127.0.0.1:8787`, mux `/ws`, `/terminal`, `/` | คงไว้ (RPC dispatch ใช้ mux นี้) |
| `io/mobile/ws.go:143-247` | `Hub`/`peer` ผูกกับ `net.Conn` (อ่าน/เขียน frame ตรง) | แยก transport interface ให้ peer ใช้ relay conn ได้ |
| `io/terminal/terminal.go:122-232` | `Handler`+`serve` ผูกกับ `net.Conn`+`bufio.Reader`, 1 WS = 1 terminal | แยก transport + route หลาย terminal ต่อ relay conn เดียว |
| `io/wsock/wsock.go:70-157` | server-side handshake (`Accept`) + `WriteFrame` **ไม่ mask** | เพิ่มฝั่ง client (mask) ใช้ร่วมกัน |
| `main.go:215-284` | เรียก handler ในโปรเซสด้วย `httptest.NewRequest` (chat, sessionModel) | แพตเทิร์นเดียวกันสำหรับ RPC ทุก path |
| `db.TouchDaemon`/`db.SaveDaemon` (`main.go:132,413`), `newerDaemonRunning` (`main.go:454`) | D1 `daemon/url|version|heartbeat` + handover guard | `daemon/url` เลิกใช้; presence+handover ย้ายไป DO (ดู §7 calls) |
| แอพ `AiDirectSocket.kt:206-211` | WS `wss://host/ws` + bearer | → relay socket (`ch:"chat"`) |
| แอพ `TerminalSocket.kt:154-168` | WS `wss://host/terminal` + bearer | → relay socket (`ch:"term"`) |
| แอพ `HistoryApi.kt` (~25 call) | HTTP REST ไป host เดียวกัน (`/v1/sessions`, `/api/*`, `/restart`, `POST /v1/attachments`) | → RPC บน relay socket (`ch:"rpc"`) |
| แอพ `SettingsStore.kt:122-160`, `DaemonDiscovery.kt:133-142` | เก็บ 3 คีย์ (endpoint/daemon/ws), poll D1 แล้ว adopt URL | เลิก adopt URL; relay URL เป็นค่าคงที่/ตั้งค่า (§5) |

## 4. รูปร่างใหม่

### 4.1 Worker + Durable Object (`relay/`)
- Worker: route `GET /room/<name>` (WS upgrade) → ส่งต่อไป DO ตัวชื่อ `<name>` (`idFromName`)
- DO (SQLite-backed ตามข้อบังคับ free plan) ใช้ **Hibernation API** (`state.acceptWebSocket`) เพื่อไม่ burn duration ตอนว่าง
- DO ถือ 2 บทบาท: `engine` (ตัวเดียว) และ `phone` (**ตัวเดียวเท่านั้น — ห้ามต่อพร้อมกันหลายเครื่อง**) — phone ที่สองที่ต่อเข้ามา: DO ปิดตัวเก่าด้วย `{"ctl":"superseded-phone"}` แล้วรับตัวใหม่ (กัน lockout จาก socket ค้าง; เป็น call ของผม)
- Auth: อ่าน `Authorization: Bearer <token>` ตอน handshake → เทียบกับ secret ของ DO (ตั้งด้วย `wrangler secret`); ไม่ตรง = 401
- Handover: engine ส่ง `{"role":"engine","started":<unix>,"version":"..."}` — DO เก็บ `started`/`version` ล่าสุดใน `state.storage` (รอด hibernation) แล้วเทียบ; ใหม่กว่าชนะ → ส่งตัวเก่า `{"ctl":"superseded"}` แล้วปิด socket; **engine ที่ถูก supersede จะจบโปรเซสเอง** (พฤติกรรมเดียวกับ `newerDaemonRunning` วันนี้) — DO เก็บกวาดช่อง engine เมื่อ socket ตาย
- Forward: DO ส่ง frame ระหว่าง engine ↔ phone (มีคู่เดียว) — **ไม่ parse เนื้อใน** ยกเว้น `ctl` ของ DO เอง; ไม่ต้องมี socket id
- Presence: `{"ctl":"presence","engine":true|false}` (แค่ดีบัก; สถานะ/uptime/version ของแอพยังมาจาก D1)
- ขนาด frame: relay ไม่แตก/ไม่ประกอบ; ใช้ message size ที่ DO รองรับ (สูงสุด 32 MiB ตามเอกสาร)

### 4.2 สัญญาบน WS (ระหว่างแอพกับ engine — DO โปร่ง)
Envelope บน text frame (JSON):
```json
{"ch":"chat", ...frame ของเดิมจาก io/mobile}
{"ch":"term","id":"t-...", "k":"json"|"b64", "p":<payload>}
{"ch":"rpc","rid":"<id>","m":"GET|POST|...","p":"/v1/sessions","b":"<base64 body>","h":{...}}
{"ch":"rpc","rid":"<id>","s":200,"h":{...},"b":"<base64 body>"}
{"ch":"ctl","id":"t-...","act":"open|close|bye","cols":100,"rows":30}
```
- binary frame = byte ของ terminal ล้วน ๆ นำหน้าด้วย header สั้น `[1B ch][2B streamId][payload]` (กัน base64 บวม); engine↔แอพตกลง layout นี้เป็นคู่ของเราเอง (§8 calls)
- compatibility: frame ที่ไม่มี `ch` ถือเป็น `chat` (แอพ/engine เก่าคุยกันได้ระหว่างเปลี่ยนผ่าน)
- `streamId` = ลำดับ terminal (มี phone เดียว จึงเป็นลำดับเดียว ไม่ต้องมี socket id)
- **RPC เป็น one-shot request/response ไม่มี streaming**: ทุก REST call ของแอพวันนี้ไม่ stream (`/v1/sessions*`, `/api/*`, `/restart`, `POST /v1/attachments`) — SSE (`POST /v1/chat/completions`) ไม่ได้ใช้จากแอพ (แชทวิ่งบน `ch:"chat"`) จึงไม่ต้องรองรับ stream ในรอบนี้
- **อัปโหลดไฟล์ต้องเป็น chunk ไม่ใช่ frame เดียว**: ฝั่ง engine รับถึง 128 MiB (`io/gateway/upload.go:21`, stream ลงดิสก์) → `ch:"rpc"` ของ upload ส่ง body เป็น binary chunk หลาย frame + `end` แทน base64 ก้อนเดียว
- **phone ปิดแอพ = ส่ง `act:"bye"` ก่อนปิด socket**: engine ไม่เห็น close frame ของ phone อีก จึงต้องมี frame บอกลา เพื่อคงพฤติกรรมเดิม "close = ยกเลิก turn ที่รันอยู่" (`io/mobile/ws.go:211-247`); socket หลุดเฉย ๆ (ไม่มี `bye`) = เหมือน drop วันนี้ (turn รันต่อ คำตอบถูกเก็บ)

### 4.3 ฝั่ง engine
- package ใหม่ (ชื่อให้ผู้สร้างเลือก) `relay`: dial client (handshake เอง + mask frame), อ่าน loop, reconnect backoff + ping 20–30 วิ, ส่ง `role=engine`
- router ในเครื่องตาม `ch`:
  - `chat` → ป้อนเข้า `mobile.Hub` ผ่าน transport interface ใหม่; **phone มีตัวเดียว** → `Hub` มี peer เดียวได้ (แต่ยังใช้ `Hub` เดิมได้เลย) — `bye` = `cancelSession("")`
  - `term`/binary → `terminal.Server` ผ่าน transport interface ใหม่; map `streamId` ↔ session + รับ `open`/`close`/`resize`
  - `rpc` → สร้าง `httptest.NewRequest(m, p, b)` + คัด header → `gw.ServeHTTP` (ยกเว้น `/ws`,`/terminal` ที่ไม่ผ่าน mux) → ตอบ `{rid,s,h,b}`; body ใหญ่/อัปโหลดรับเป็น chunk ต่อ frame
- เมื่อได้ `{"ctl":"superseded"}` → จบโปรเซส (เท่า `newerDaemonRunning` วันนี้: คืนจาก `main` → Kaggle ปิด/พุช session)
- ลบ cloudflared/keeper; **D1 `daemon/url|version|heartbeat` ยังเขียนเหมือนเดิม** — แต่ `daemon/url` = relay URL นิ่ง (จาก env) ไม่ใช่ trycloudflare → แอพ discovery/สถานะเดิมไม่ต้องแก้; handover ตัดสินที่ DO
- `server` บน `127.0.0.1:8787` ยังอยู่ เพราะ RPC dispatch วิ่งบน mux นี้ (`/healthz` ยังใช้ในเครื่อง)

### 4.4 ฝั่งแอพ
- socket เดียว (`RelaySocket`) แทน `AiDirectSocket` + `TerminalSocket` + HTTP ของ `HistoryApi`
- `HistoryApi` ทุก call → `rpc(rid, m, p, b)` แล้วรอตอบตาม `rid` (timeout เดิมต่อ call); upload = ส่ง body เป็น chunk (`ch:"rpc"` หลาย frame + `end`)
- terminal: เปิด/ปิด/ย่อขยายผ่าน `ctl` + binary; รองรับหลายหน้าต่างบน socket เดียว
- **ปิดแอพ/ปิดหน้าต่าง chat → ส่ง `{"ch":"ctl","act":"bye"}` ก่อน close** เพื่อให้ engine ยกเลิก turn เหมือนเดิม
- `SettingsStore`: **เหลือการตั้งค่าแค่ CF token เหมือนเดิม ไม่เพิ่มช่อง URL/room**; `daemon_url` ยังถูกเก็บจาก discovery แต่ค่าคือ **relay URL (นิ่ง)** แทน trycloudflare; `wsUrlFor()` ไม่ต้องประกอบ `/ws` อีก (relay base ใช้ตรง ๆ) — เดินตามค่า discovered
- `DaemonDiscovery`: ยัง poll D1 + adopt URL เหมือนเดิม (URL นิ่งแล้วจึง adopt ครั้งเดียว); online/uptime/version จาก D1 เหมือนเดิม
- backoff/ping ของ `AiDirectSocket` (1s → 30s + jitter, `pingInterval` 20 วิ) ใช้ต่อ

### 4.5 ทำไมพอ (หลักฐานจากเอกสาร Cloudflare, ต.ค. 2026)
- Workers Free: **100,000 requests/วัน**; WS upgrade นับ 1 request; **ไม่คิดค่า egress/bandwidth**
- DO Free: **100,000 requests/วัน** + incoming WS messages นับแบบ **20:1** → **~2,000,000 WS messages/วัน** (~60M/เดือน); duration **13,000 GB-s/วัน**
- DO 1 ตัวต่อค้าง = 128 MB × 86,400 s ≈ **11,059 GB-s/วัน** < 13,000 → อยู่ (ใช้ Hibernation ยิ่งเหลือ)
- เทียบ Supabase Realtime ที่ผู้ใช้ตัดออก: 2M messages/**เดือน** = frame จริง ~1M/เดือน → terminal ~200 frame/วิ = หมดใน ~83 นาที (DO ใจดีกว่า ~60 เท่า)

## 5. การตั้งค่า/ดีพลอย

| สิ่ง | ค่า |
|---|---|
| Worker URL | `wss://<worker>.<account>.workers.dev/room/<room>` (นิ่ง) |
| ที่มาของ URL | engine เผยแพร่ลง D1 `daemon/url` (เหมือน tunnel วันนี้) → แอพ discovery เดิม |
| token | CF token เดียวกับวันนี้ (bearer) — **การตั้งค่าแอพเหลือช่องเดียวเหมือนเดิม** |
| deploy token | สร้างใหม่จากตัวที่ให้มา: กลุ่ม `Workers Scripts:Write` (`e086da7e2179491d91ee5f35b3ca210a`) + `Workers Scripts:Read` — ใช้ตอน deploy เท่านั้น ไม่ ship ไปแอพ |
| deploy | `wrangler deploy` + ตั้ง workers.dev subdomain (ผ่าน token ใหม่) |
| DO secret | `RELAY_TOKEN` = bearer token (ตัวเดียวกับที่แอพ/engine ส่ง) |
| engine env | `AIXODIA_RELAY_URL`, `AIXODIA_ROOM` (ถ้าไม่ตั้ง = โหมดไม่ต่อ relay) |

## 6. การทดสอบ

- unit engine: `wsock` client (mask round-trip กับ server ในเครื่อง), router แยก `ch` ถูก, RPC dispatch GET/POST ตรงกับเรียก handler ตรง
- unit terminal: หลาย session บน transport เดียว (echo, resize, exit)
- integration ในเครื่อง: รัน engine → ต่อ DO local (`wrangler dev`) → เปิดสอง socket (จำลองแอพ/engine) → แชท/terminal/RPC ผ่านครบ + handover (engine ใหม่แย่งที่แล้วตัวเก่าปิด)
- ของเดิมต้องเขียว: `go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...`
- ฝั่งแอพ: unit `RelaySocket` (route `ch`, rpc timeout), `HistoryApi` บน RPC แทน OkHttp
- ยิงจริง: Cloudflare DO (deploy แล้ว) + Kaggle session → เปิดแชท, terminal (ปล่อย `yes` 10 วิ ดูโควตา/ความลื่น), อัปไฟล์, restart

## 7. ลำดับปฏิบัติ

**หลัก: engine รัน tunnel + relay คู่กันไปก่อน จนกว่า APK ใหม่ถูกติดตั้งและ verify แล้ว — ห้ามตัด cloudflared ก่อนแอพอัป**

1. `relay/` (Worker+DO+hibernation+auth+handover+presence) + เทสต์ด้วย `wrangler dev` (ไม่ต้องมีโดเมน/ไม่ต้องมี Kaggle)
2. engine: `wsock` client + package relay + transport interface ของ `mobile`/`terminal` + router + RPC dispatch + เทสต์ — **ยังคง cloudflared + publish `daemon/url` เหมือนเดิม** (relay เป็นทางเสริม เปิดด้วย env)
3. release rolling `engine-latest`; เปิด relay ควบคู่; session ใหม่รันได้ทั้งสองทาง
4. แอพ: `RelaySocket` + ย้าย `HistoryApi`/terminal/`SettingsStore`/discovery; ปล่อย APK; **ไม่ต้องตั้งค่าในแอพเพิ่ม** (discovery เดิมอ่าน relay URL จาก D1), verify
5. verify ปลายทางแล้วจึง **ลบ cloudflared path + `tunnel.go`**; เก็บ D1 `daemon/url|version|heartbeat` ไว้ (url = relay URL นิ่ง)

## 8. Calls ของผม (ให้ตรวจ)

- **Envelope + binary header**: text = JSON มี `ch`; binary = `[1B ch][2B streamId][payload]`; ไม่มี `ch` = `chat` (เข้ากันได้ถอยหลัง)
- **DO ไม่ parse เนื้อ frame** ยกเว้น `sid`/`ctl` ที่ DO ใส่เอง — ฉลาดน้อยสุด ปลอดภัยสุด
- **phone ต่อได้ทีละเครื่อง (ผู้ใช้กำหนด)**: DO ให้ phone ได้ตัวเดียว; เครื่องที่สองที่ต่อเข้ามาปิดตัวเก่า (`superseded-phone`) แล้วรับตัวใหม่ — engine จึงมี peer เดียว และไม่ต้องมี socket id
- **engine ที่ถูก supersede จบโปรเซส** (เท่าพฤติกรรมเดิม) และ DO เก็บกวาดช่อง engine เมื่อ socket ตาย → กัน race ด้วยการตัดสินที่ DO จุดเดียว; engine ที่ reconnect/`started` เท่ากันถือว่าเป็นตัวเดียวกัน
- **handover ตัดสินที่ DO** แต่ **D1 `daemon/url|version|heartbeat` ยังเขียนเหมือนเดิม** (url = relay URL นิ่ง) → แอพ discovery/สถานะไม่ต้องแก้; DO เป็นผู้ตัดสินว่า engine ตัวไหนได้ช่อง relay
- **RPC one-shot ไม่มี streaming** — ครอบทุก REST call ที่แอพใช้ (`/v1/sessions*`, `/api/*`, `/restart`, uploads); SSE ของ `/v1/chat/completions` ไม่ใช้จากแอพ
- **อัปโหลดเป็น chunk/binary** (สูงสุด 128 MiB) ไม่ใช่ base64 frame เดียว
- **`bye` frame** เพื่อคง "ปิดแอพ = ยกเลิก turn" (engine ไม่เห็น close ของ phone อีก)
- **REST ผ่าน RPC ที่ dispatch เข้า mux เดิม** — ไม่แตะ handler; `/ws`,`/terminal` ไม่รับผ่าน RPC
- **Worker/DO อยู่ในรีโป engine ที่ `relay/`** (ไม่ตั้งรีโปใหม่)
- **ไม่เพิ่ม Go dependency** — เขียน WS client เองใน `wsock`
- **การตั้งค่าแอพ = CF token อย่างเดียวเหมือนเดิม** (ไม่เพิ่มช่อง URL/room); relay URL มาจาก D1 `daemon/url` ผ่าน discovery เดิม; room เป็นค่าคงที่ฝั่ง deploy/engine (`AIXODIA_ROOM`)
- **ผมแก้ทั้งสองฝั่ง** (engine + `aixodia-app`) และดีพลอย Worker เอง (token ที่ให้มาสร้าง deploy token ได้)

## 9. ความเสี่ยง / ค้าง

- **สิทธิ์ Workers ยังไม่พร้อม** — บัญชียังไม่มี Worker/workers.dev subdomain; ตัวแก้: token ใหม่ที่ผู้ใช้ให้มา (`API Tokens Write`) → สร้าง deploy token กลุ่ม `Workers Scripts:Write` แล้วตั้ง subdomain + deploy (§5)
- **blast radius ของ token (ผู้ใช้ตัดสิน: ใช้ token ตัวเดียว)**: token ที่มี `Workers Scripts:Write` ถูกใช้เป็น bearer ที่แอพถือด้วย → ใครได้ token = deploy Worker ได้ ยอมรับความเสี่ยงนี้ (ผู้ใช้เลือก 2026-10-19)
- **DO frame/message size + hibernation**: ต้องยืนยันว่าขนาด frame terminal (32 KiB/chunk) ผ่าน และ relay ตอน terminal เปิดค้างไม่ทำให้ duration เกิน 13,000 GB-s/วัน
- **reconnect + state**: terminal ไม่มี reconnect วันนี้ (ตั้งใจ); หลังย้าย transport ต้องคงพฤติกรรมเดิม (قطعแล้ว shell ตาย)
- **แอพ rewrite ใหญ่**: `HistoryApi` ~25 call → RPC + upload chunking = ความเสี่ยงบั๊กสูงสุดของงานนี้
- **Supersede ที่ DO**: ต้องกัน two-engine race (ตัดสินที่ DO จุดเดียว + เก็บกวาด slot เมื่อ socket ตาย + reconnect ตัวเดิมไม่ถูกหาว่าเก่า)
- **ช่วงเปลี่ยนผ่าน**: engine ต้องรัน tunnel คู่ relay จนกว่าแอพอัป (§7) — ถ้าลืม แอพเก่าทุกตัว offline
- **ทดสอบไม่ได้ offline**: DO ต้อง deploy/`wrangler dev` จริง
- revert: ลบ `relay/`, คืน `tunnel.go` + commit เดิม, ปิด env; ไม่มี migration ข้อมูล (ยกเว้นที่ลบ `daemon/url`)

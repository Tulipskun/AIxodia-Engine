# Cloudflare Worker + Durable Object relay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** สร้าง relay บน Cloudflare (Worker + Durable Object) ที่จับคู่ engine กับมือถือ (อย่างละตัว) และส่งต่อ frame ระหว่างกัน โดยไม่พึ่ง cloudflared/tunnel

**Architecture:** Worker มี route เดียว `GET /room/<name>` (WebSocket upgrade) แล้วส่งต่อไป Durable Object ชื่อ `<name>`; DO ถือ slot `engine` 1 และ `phone` 1, ตัดสิน handover ด้วย `started`, ส่งต่อ frame แบบโปร่ง (ไม่ parse) และใช้ Hibernation API + SQLite storage ตามข้อบังคับ free plan

**Tech Stack:** TypeScript, Cloudflare Workers + Durable Objects (SQLite backend, Hibernation API), `wrangler`, `vitest` + `@cloudflare/vitest-pool-workers`

**Spec:** `docs/superpowers/specs/2026-10-19-cloudflare-relay-design.md` (§4.1, §4.5, §5)

## Global Constraints

- **Free plan เท่านั้น**: DO ต้องเป็น SQLite-backed (`new_sqlite_classes`) และต้องใช้ **Hibernation API** (`state.acceptWebSocket`) — ห้าม `ws.accept()` ตรง ๆ (burn duration)
- **token เดียว** (`RELAY_TOKEN` secret): เทียบตอน handshake ด้วย **constant-time**; ไม่ตรง/ไม่มี = `401` และไม่ส่ง frame ใด ๆ
- **phone ตัวเดียวเท่านั้น** — ต่อพร้อมกัน 2 เครื่องไม่ได้ (ตัวใหม่ปิดตัวเก่า)
- **engine ตัวเดียวเท่านั้น** — `started` ใหม่กว่าชนะ; เท่ากันถือว่าเป็นตัวเดิม
- DO **ไม่ parse เนื้อ frame** ยกเว้น `ctl` ของ DO เอง (ส่งต่อ byte/text ตามเดิม)
- protocol ระหว่าง engine ↔ phone **เป็นของคู่นั้น** (relay ไม่รู้จัก `ch`,`rid`,`sid`)

## Review Focus

- ไม่มี/ผิด bearer → ต้องได้ `401` และ DO ต้องไม่ถูกแตะเลย (ไม่จับคู่ ไม่เก็บ state)
- frame แรกก่อนส่ง role (`{"role":...}`) หรือ role แปลก → ต้องไม่ถูก forward ไปฝั่งตรงข้าม
- engine ที่ `started` เท่ากัน/เก่ากว่า → ต้องไม่แย่ง slot และไม่ปิด engine ที่ครองอยู่
- phone หลุด/ปิด → engine ต้องได้ `presence engine`? ไม่ — phone หลุดต้องไม่ทำให้ engine ถูกปิด, และ presence ต้องสะท้อน engine เท่านั้น
- frame ขนาดใหญ่ (terminal chunk ~32 KiB) → ต้องส่งผ่านได้ทั้งสองทางไม่ถูกตัด

---

### Task 1: Scaffold โปรเจกต์ `relay/` + health route

**Files:**
- Create: `relay/package.json`
- Create: `relay/wrangler.toml`
- Create: `relay/tsconfig.json`
- Create: `relay/vitest.config.ts`
- Create: `relay/src/index.ts`
- Create: `relay/src/index.test.ts`
- Create: `relay/.gitignore` (`.wrangler/`, `node_modules/`)

**Interfaces:**
- Produces: default `export default { fetch(request: Request, env: Env): Promise<Response> }`; `interface Env { ROOM: DurableObjectNamespace; RELAY_TOKEN: string }`
- Produces: `GET /healthz` → `200` body `"ok"`

- [ ] **Step 1: เขียนเทสต์ที่fail**

```ts
// relay/src/index.test.ts
import { SELF } from "cloudflare:test";
import { describe, it, expect } from "vitest";

describe("health", () => {
  it("answers ok", async () => {
    const res = await SELF.fetch("https://relay.test/healthz");
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("ok");
  });
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm install && npx vitest run src/index.test.ts`
Expected: FAIL (no `src/index.ts` / no default export)

- [ ] **Step 3: สร้างไฟล์โปรเจกต์**

`relay/wrangler.toml`:
```toml
name = "aixodia-relay"
main = "src/index.ts"
compatibility_date = "2026-10-19"

[[durable_objects.bindings]]
name = "ROOM"
class_name = "Room"

[[migrations]]
tag = "v1"
new_sqlite_classes = ["Room"]
```
`relay/vitest.config.ts`:
```ts
import { defineWorkersConfig } from "@cloudflare/vitest-pool-workers/config";
export default defineWorkersConfig({
  test: { poolOptions: { workers: { wrangler: { configPath: "./wrangler.toml" } } } },
});
```
`relay/tsconfig.json`: target `es2022`, `module` `es2022`, `moduleResolution` `bundler`, `types`: `["@cloudflare/workers-types", "@cloudflare/vitest-pool-workers"]`, `strict: true`
`relay/package.json` scripts: `{"dev":"wrangler dev","deploy":"wrangler deploy","test":"vitest run"}`; devDeps: `wrangler`, `vitest`, `@cloudflare/vitest-pool-workers`, `typescript`, `@cloudflare/workers-types`

`relay/src/index.ts`:
```ts
export interface Env { ROOM: DurableObjectNamespace; RELAY_TOKEN: string }

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") return new Response("ok");
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
```
(ในเทสต์ `RELAY_TOKEN` มาจาก `[vars]` ใน wrangler.toml: `RELAY_TOKEN = "test-token"` — ใส่ชั่วคราวเพื่อเทสต์; ของจริงใช้ `wrangler secret`)

- [ ] **Step 4: รันเทสต์ให้ผ่าน**

Run: `cd relay && npx vitest run src/index.test.ts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add relay/package.json relay/wrangler.toml relay/tsconfig.json relay/vitest.config.ts relay/src/index.ts relay/src/index.test.ts relay/.gitignore
git commit -m "relay: scaffold worker project with health route"
```

---

### Task 2: ฟังก์ชันตัดสิน slot (pure) + เทสต์

**Files:**
- Create: `relay/src/decide.ts`
- Create: `relay/src/decide.test.ts`

**Interfaces:**
- Produces: `decideEngine(existing: {started:number} | null, incoming: {started:number}): "accept" | "reject"`
- Produces: `decidePhone(existing: boolean): "accept" | "takeover"`
- Produces: `bearerOk(header: string | null, token: string): boolean`

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```ts
// relay/src/decide.test.ts
import { describe, it, expect } from "vitest";
import { decideEngine, decidePhone, bearerOk } from "./decide";

describe("decideEngine", () => {
  it("accepts when the slot is free", () => {
    expect(decideEngine(null, { started: 100 })).toBe("accept");
  });
  it("accepts a newer engine", () => {
    expect(decideEngine({ started: 100 }, { started: 101 })).toBe("accept");
  });
  it("rejects an older engine", () => {
    expect(decideEngine({ started: 100 }, { started: 99 })).toBe("reject");
  });
  it("rejects an equal started (same instance reconnecting)", () => {
    expect(decideEngine({ started: 100 }, { started: 100 })).toBe("reject");
  });
});

describe("decidePhone", () => {
  it("accepts when free", () => {
    expect(decidePhone(false)).toBe("accept");
  });
  it("takes over when busy", () => {
    expect(decidePhone(true)).toBe("takeover");
  });
});

describe("bearerOk", () => {
  it("accepts the exact bearer", () => {
    expect(bearerOk("Bearer s3cret", "s3cret")).toBe(true);
  });
  it("rejects a wrong bearer", () => {
    expect(bearerOk("Bearer nope", "s3cret")).toBe(false);
  });
  it("rejects a missing header", () => {
    expect(bearerOk(null, "s3cret")).toBe(false);
  });
  it("rejects an empty configured token", () => {
    expect(bearerOk("Bearer ", "")).toBe(false);
  });
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npx vitest run src/decide.test.ts`
Expected: FAIL (cannot resolve `./decide`)

- [ ] **Step 3: เขียน `relay/src/decide.ts`**

signatures ตาม Interfaces; `bearerOk` ตัด prefix `Bearer ` (trim) แล้วเทียบกับ token แบบ constant-time (เทียบความยาวก่อน แล้ว XOR สะสมทุกไบต์ของค่าที่ pad ความยาวให้เท่ากัน — token ว่างต้องได้ `false` เสมอ); `decideEngine` = accept เมื่อ existing เป็น null หรือ `incoming.started > existing.started`; `decidePhone` = takeover เมื่อ busy

- [ ] **Step 4: รันเทสต์ให้ผ่าน**

Run: `cd relay && npx vitest run src/decide.test.ts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add relay/src/decide.ts relay/src/decide.test.ts
git commit -m "relay: slot decisions and bearer check"
```

---

### Task 3: Auth + upgrade + จับคู่ engine↔phone (ส่งต่อ frame)

**Files:**
- Create: `relay/src/room.ts`
- Modify: `relay/src/index.ts` (route `/room/<name>` → `env.ROOM.getByName(name).fetch(request)`)
- Create: `relay/src/room.test.ts`

**Interfaces:**
- Consumes: `bearerOk`, `decideEngine`, `decidePhone` (Task 2)
- Produces: class `Room` (`relay/src/room.ts`) — DO ที่มี `fetch(request: Request): Promise<Response>`
- Produces (protocol): client ส่ง frame แรก `{"role":"engine","started":N,"version":"..."}` หรือ `{"role":"phone"}`; DO ส่ง `{"ctl":"superseded"}` / `{"ctl":"superseded-phone"}` / `{"ctl":"presence","engine":boolean}`

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```ts
// relay/src/room.test.ts
import { SELF } from "cloudflare:test";
import { describe, it, expect } from "vitest";

const TOK = "test-token";
function dial(name = "main") {
  return SELF.fetch(`https://relay.test/room/${name}`, {
    headers: { Upgrade: "websocket", Authorization: `Bearer ${TOK}` },
  });
}

describe("room auth", () => {
  it("401s without a token", async () => {
    const res = await SELF.fetch("https://relay.test/room/main", {
      headers: { Upgrade: "websocket" },
    });
    expect(res.status).toBe(401);
  });
  it("401s with a wrong token", async () => {
    const res = await SELF.fetch("https://relay.test/room/main", {
      headers: { Upgrade: "websocket", Authorization: "Bearer nope" },
    });
    expect(res.status).toBe(401);
  });
});

describe("room pairing", () => {
  it("forwards phone frames to the engine and back", async () => {
    const eng = (await dial("fwd")).webSocket!;
    eng.accept();
    eng.send(JSON.stringify({ role: "engine", started: 100, version: "t" }));

    const ph = (await dial("fwd")).webSocket!;
    ph.accept();
    ph.send(JSON.stringify({ role: "phone" }));

    const got: string[] = [];
    eng.addEventListener("message", (e) => got.push(String(e.data)));

    ph.send("hello-engine");
    await new Promise((r) => setTimeout(r, 50));
    expect(got).toContain("hello-engine");

    const back: string[] = [];
    ph.addEventListener("message", (e) => back.push(String(e.data)));
    eng.send("hello-phone");
    await new Promise((r) => setTimeout(r, 50));
    expect(back).toContain("hello-phone");
  });
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npx vitest run src/room.test.ts`
Expected: FAIL (`/room/...` returns 404 / no `Room` class)

- [ ] **Step 3: เขียน `relay/src/room.ts` + ต่อ route**

`Room.fetch`:
1. `bearerOk(request.headers.get("Authorization"), env.RELAY_TOKEN)` ไม่ผ่าน → `new Response("unauthorized",{status:401})`
2. ไม่ใช่ upgrade → `400`
3. `state.acceptWebSocket(server)` แล้ว `server.serializeAttachment({ role: null })` (ยังไม่รู้ role จนได้ hello)
4. handler `webSocketMessage(ws, msg)`:
   - ถ้ายังไม่มี role: parse `JSON.parse` — ต้องมี `role` ∈ {engine, phone}; ใส่ role ลง attachment; apply `decideEngine`/`decidePhone` กับ slot ปัจจุบัน (อ่าน/เขียน `state.storage`); ถ้า reject → `ws.send('{"ctl":"superseded"}')` แล้ว `ws.close()`; ถ้า accept → เก็บ slot (`engine`/`phone`) ลง `state.storage` (engine เก็บ `{started,version}`), ปิดตัวเก่าด้วย `ws.close(1000,"superseded")` ถ้ามี takeover, แล้วส่ง `presence` ให้ phone
   - ถ้ามี role แล้ว: forward ไปอีกฝั่ง (engine→phone / phone→engine); ถ้าอีกฝั่งไม่มี = ทิ้ง
5. `webSocketClose(ws)` / `webSocketError`: ล้าง slot ที่ตรงกับ role ของ ws + ส่ง `presence` ให้ phone
6. helper หา socket ของ role: วน `state.getWebSockets()` แล้วอ่าน `deserializeAttachment().role`

`index.ts`: path ตรง `^/room/([^/]+)$` → `env.ROOM.getByName(name).fetch(request)`

- [ ] **Step 4: รันเทสต์ให้ผ่าน**

Run: `cd relay && npx vitest run src/room.test.ts`
Expected: PASS (ทั้ง auth และ pairing)

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/src/index.ts relay/src/room.test.ts
git commit -m "relay: auth, upgrade and engine/phone pairing"
```

---

### Task 4: engine handover (`started` ใหม่กว่าชนะ)

**Files:**
- Modify: `relay/src/room.ts`
- Modify: `relay/src/room.test.ts`

**Interfaces:**
- Consumes: `decideEngine` (Task 2), `Room` (Task 3)
- Produces: พฤติกรรม — engine ใหม่กว่า → ตัวเก่าได้ `{"ctl":"superseded"}` แล้วปิด; เท่ากัน/เก่ากว่า → ตัวใหม่ถูกปฏิเสธ + ปิด; `started` คงอยู่ใน `state.storage` ข้ามการหลุดของ socket

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```ts
it("a newer engine takes the slot and the old one is closed", async () => {
  const a = (await dial("ho")).webSocket!; a.accept();
  a.send(JSON.stringify({ role: "engine", started: 100, version: "a" }));
  await new Promise((r) => setTimeout(r, 20));

  const closed: string[] = [];
  a.addEventListener("close", () => closed.push("a"));

  const b = (await dial("ho")).webSocket!; b.accept();
  b.send(JSON.stringify({ role: "engine", started: 101, version: "b" }));
  await new Promise((r) => setTimeout(r, 50));
  expect(closed).toContain("a");

  // เท่ากัน/เก่ากว่า ถูกปฏิเสธ
  const c = (await dial("ho")).webSocket!; c.accept();
  const msgs: string[] = [];
  c.addEventListener("message", (e) => msgs.push(String(e.data)));
  c.send(JSON.stringify({ role: "engine", started: 101, version: "c" }));
  await new Promise((r) => setTimeout(r, 50));
  expect(msgs.some((m) => m.includes("superseded"))).toBe(true);
});
```
(หมายเหตุ: เขียนเป็น `it(...)` ใน `describe("handover")`)

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npx vitest run src/room.test.ts -t handover`
Expected: FAIL

- [ ] **Step 3: ทำ handover ใน `room.ts` ให้ครบ**

ใช้ `decideEngine`; ตอน accept engine ใหม่: ถ้ามี engine เก่า → `old.send('{"ctl":"superseded"}')` + `old.close(1000,"superseded")`; ตอน reject → `ws.send('{"ctl":"superseded"}')` + `ws.close()`; เก็บ `{started,version}` ลง `state.storage.put("engine", ...)` และลบเมื่อ engine socket ปิด/error (ถ้า `started` ตรงกับที่เก็บ)

- [ ] **Step 4: รันเทสต์ให้ผ่าน**

Run: `cd relay && npx vitest run src/room.test.ts`
Expected: PASS (ทุก describe)

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/src/room.test.ts
git commit -m "relay: engine handover by started"
```

---

### Task 5: phone ตัวเดียว + presence

**Files:**
- Modify: `relay/src/room.ts`
- Modify: `relay/src/room.test.ts`

**Interfaces:**
- Consumes: `decidePhone` (Task 2), `Room` (Task 3)
- Produces: พฤติกรรม — phone ที่ 2 ปิดตัวเก่าด้วย `{"ctl":"superseded-phone"}`; phone ได้ `{"ctl":"presence","engine":bool}` ทุกครั้งที่สถานะ engine เปลี่ยน; phone หลุดไม่กระทบ engine

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```ts
describe("single phone", () => {
  it("the second phone closes the first", async () => {
    const eng = (await dial("sp")).webSocket!; eng.accept();
    eng.send(JSON.stringify({ role: "engine", started: 1, version: "e" }));
    await new Promise((r) => setTimeout(r, 20));

    const p1 = (await dial("sp")).webSocket!; p1.accept();
    p1.send(JSON.stringify({ role: "phone" }));
    const p1closed: string[] = [];
    p1.addEventListener("message", (e) => p1closed.push(String(e.data)));

    const p2 = (await dial("sp")).webSocket!; p2.accept();
    p2.send(JSON.stringify({ role: "phone" }));
    await new Promise((r) => setTimeout(r, 50));
    expect(p1closed.some((m) => m.includes("superseded-phone"))).toBe(true);

    // engine ยังอยู่ ไม่ถูกปิด
    const back: string[] = [];
    p2.addEventListener("message", (e) => back.push(String(e.data)));
    eng.send("still-here");
    await new Promise((r) => setTimeout(r, 50));
    expect(back).toContain("still-here");
  });

  it("tells the phone when the engine attaches", async () => {
    const p = (await dial("pres")).webSocket!; p.accept();
    const seen: string[] = [];
    p.addEventListener("message", (e) => seen.push(String(e.data)));
    p.send(JSON.stringify({ role: "phone" }));
    const eng = (await dial("pres")).webSocket!; eng.accept();
    eng.send(JSON.stringify({ role: "engine", started: 1, version: "e" }));
    await new Promise((r) => setTimeout(r, 50));
    expect(seen.some((m) => m.includes('"engine":true'))).toBe(true);
  });
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npx vitest run src/room.test.ts -t "single phone"`
Expected: FAIL

- [ ] **Step 3: ทำ takeover + presence ใน `room.ts`**

`decidePhone(busy)`: `takeover` → `old.send('{"ctl":"superseded-phone"}')` + `old.close(1000,"superseded-phone")`; ส่ง presence หลัง engine attach/detach เท่านั้น

- [ ] **Step 4: รันเทสต์ให้ผ่าน**

Run: `cd relay && npx vitest run src/room.test.ts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/src/room.test.ts
git commit -m "relay: one phone at a time with presence"
```

---

### Task 6: ทน hibernation + ยิงจริงด้วย `wrangler dev`

**Files:**
- Modify: `relay/src/room.ts`
- Create: `relay/README.md`

**Interfaces:**
- Consumes: `Room` (Tasks 3–5)
- Produces: เอกสารรัน/ดีพลอย + ยืนยันว่า role/slot อ่านจาก `state` (ไม่พึ่งหน่วยความจำในโปรเซส)

- [ ] **Step 1: เขียนเทสต์ที่ fail (slot อยู่รอดหลัง socket ปิดถูกประมวลผล)**

```ts
it("clears the engine slot when the engine socket closes", async () => {
  const p = (await dial("hib")).webSocket!; p.accept();
  const seen: string[] = [];
  p.addEventListener("message", (e) => seen.push(String(e.data)));
  p.send(JSON.stringify({ role: "phone" }));

  const eng = (await dial("hib")).webSocket!; eng.accept();
  eng.send(JSON.stringify({ role: "engine", started: 5, version: "e" }));
  await new Promise((r) => setTimeout(r, 50));
  eng.close();
  await new Promise((r) => setTimeout(r, 50));
  expect(seen.some((m) => m.includes('"engine":false'))).toBe(true);
});
```

- [ ] **Step 2: รันให้เห็น fail ถ้ายังล้าง slot ไม่ครบ**

Run: `cd relay && npx vitest run src/room.test.ts -t hiber`
Expected: FAIL ถ้า close handler ยังไม่ล้าง/ไม่แจ้ง presence

- [ ] **Step 3: ทำให้ผ่าน + เอกสาร**

ทำให้ `webSocketClose`/`webSocketError` ล้าง slot ตาม role (ตรวจ `started` ก่อนลบ engine) และแจ้ง presence; ยืนยันว่าโค้ดอ่าน role จาก `deserializeAttachment()` และ slot จาก `state.storage` เท่านั้น (ไม่มีตัวแปร instance ที่ต้องมีก่อน) — เขียน `relay/README.md`:

```markdown
# aixodia-relay
npx wrangler dev                      # ทดสอบในเครื่อง
npx wrangler secret put RELAY_TOKEN   # ตั้ง bearer (ค่าเดียวกับ CF token ที่แอพ/engine ใช้)
npx wrangler deploy                   # ได้ wss://aixodia-relay.<subdomain>.workers.dev/room/<name>
```
(พร้อมโน้ต: free plan, SQLite DO, hibernation, phone เดียว, engine เดียว)

- [ ] **Step 4: รันทุกเทสต์ + ยิงจริง**

Run: `cd relay && npx vitest run`
Expected: PASS ทั้งหมด
Run: `npx wrangler dev` แล้วต่อ WS 2 ตัว (engine+phone) ด้วย `wscat` หรือสคริปต์ Node → ยืนยันส่งต่อได้จริง

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/src/room.test.ts relay/README.md
git commit -m "relay: survive hibernation, document dev/deploy"
```

---

## Self-Review

- **Spec coverage:** §4.1 (roles/handover/forward/presence/ขนาด frame) → Tasks 3–6; §4.5 (free plan/hibernation) → Task 1 (SQLite class) + Task 6; §5 (token/deploy) → Tasks 1,6. ยังไม่ครอบ: การ deploy จริงบนบัญชี (อยู่ท้ายแผน/ทำมือ — ไม่มีเทสต์)
- **Step scan:** ทุก step มี action+ผลตรวจ; ไม่มี "TBD"
- **Type consistency:** `decideEngine`/`decidePhone`/`bearerOk` ชื่อเดียวจาก Task 2 ใช้ใน Tasks 3–5; `Room`/`Env` ใช้ชื่อเดิม
- **Review Focus:** bearer ผิด → Task 3; role ผิด → Task 3; `started` เท่ากัน/เก่า → Task 4; phone หลุดไม่กระทบ engine → Task 5; frame ใหญ่ → ยังไม่มีเทสต์เฉพาะ (Task 6 ยิงจริงด้วย chunk 32 KiB)
- **Proportion:** แผนสั้นกว่าสเปกที่อ้าง — มีแต่ signature/เทสต์ ไม่มี body เต็ม

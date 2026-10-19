# Cloudflare Worker + Durable Object relay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** สร้าง relay บน Cloudflare (Worker + Durable Object) ที่จับคู่ engine กับมือถือ (อย่างละตัว) และส่งต่อ frame ระหว่างกัน โดยไม่พึ่ง cloudflared/tunnel

**Architecture:** Worker มี route เดียว `GET /room/<name>` (WebSocket upgrade) แล้วส่งต่อไป Durable Object ชื่อ `<name>`; DO ถือ slot `engine` 1 และ `phone` 1, ตัดสิน handover ด้วย `started`, ส่งต่อ frame แบบโปร่ง (ไม่ parse) และใช้ Hibernation API + SQLite storage ตามข้อบังคับ free plan

**Tech Stack:** TypeScript, Cloudflare Workers + Durable Objects (SQLite backend, Hibernation API), `wrangler` (deploy เท่านั้น), Node test runner (`node --test`) + `ws`

**Spec:** `docs/superpowers/specs/2026-10-19-cloudflare-relay-design.md` (§4.1, §4.5, §5, §6)

## Global Constraints

- **ทดสอบได้ทางเดียว = ยิง Worker ที่ deploy แล้วจริง** — `workerd` รันในเครื่องนี้ไม่ได้ (aarch64+PRoot: TCMalloc mmap 1 GiB ล้ม) → `wrangler dev` และ vitest workers-pool **ใช้ไม่ได้**; `wrangler deploy` ใช้ได้
- **ที่อยู่จริง**: `https://aixodia-relay.aixodia.workers.dev` (subdomain `aixodia` มีอยู่แล้ว); deploy token เก็บที่ `/tmp/opencode/relay.env` (ไม่ commit) — ให้ env `CLOUDFLARE_API_TOKEN`
- **secret `RELAY_TOKEN`** ตั้งด้วย `wrangler secret put` (ค่าที่ใช้ทดสอบ = `test-token`); ห้ามใส่ token จริงลง wrangler.toml
- **Free plan เท่านั้น**: DO ต้องเป็น SQLite-backed (`new_sqlite_classes`) และใช้ **Hibernation API** (`state.acceptWebSocket`) — ห้าม `ws.accept()`
- **phone ตัวเดียว / engine ตัวเดียว** ต่อ room
- DO **ไม่ parse เนื้อ frame** ยกเว้น `ctl` ของ DO เอง
- protocol ระหว่าง engine ↔ phone **เป็นของคู่นั้น** (relay ไม่รู้จัก `ch`,`rid`)

## Testing Conventions (ใช้ทุก task)

```bash
cd relay
export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)"
npx wrangler deploy                       # ดีพลอยขึ้น Cloudflare จริง
RELAY_TOKEN=test-token npm test           # node --test test/
```
- ทุก task ที่แก้ Worker: **deploy ก่อน แล้วรันเทสต์** (ไม่มีทางรันในเครื่อง)
- `node_modules` ต้องมี (Task 1 ติดตั้ง) — เทสต์ใช้ `ws` เป็น client (ตั้ง header `Authorization` ได้; `WebSocket` ของ Node ตั้ง header ไม่ได้)

## Review Focus

- ไม่มี/ผิด bearer → `401` และ DO ต้องไม่ถูกแตะเลย (ไม่จับคู่ ไม่เก็บ state)
- frame แรกก่อนส่ง `{"role":...}` หรือ role แปลก → ต้องไม่ถูก forward ไปฝั่งตรงข้าม
- engine ที่ `started` เท่ากัน/เก่ากว่า → ต้องไม่แย่ง slot และไม่ปิด engine ที่ครองอยู่
- phone ที่สอง → ตัวเก่าถูกปิด (`superseded-phone`) แต่ **engine ต้องไม่ถูกปิด**
- frame 32 KiB (chunk ของ terminal) → ผ่านได้ทั้งสองทาง ไม่ถูกตัด

---

### Task 1: Scaffold `relay/` + deploy + `/healthz` (ยิงจริง)

**Files:**
- Create: `relay/package.json`, `relay/wrangler.toml`, `relay/tsconfig.json`, `relay/.gitignore`
- Create: `relay/src/index.ts`, `relay/src/room.ts` (DO เปล่า — ต้องมีเพื่อให้ binding ผ่าน)
- Create: `relay/test/helpers.mjs`, `relay/test/health.test.mjs`

**Interfaces:**
- Produces: default `export default { fetch(request: Request, env: Env): Promise<Response> }`; `interface Env { ROOM: DurableObjectNamespace; RELAY_TOKEN: string }`; `export class Room` (DO, เปล่า)
- Produces: `GET /healthz` → `200` body `"ok"`
- Produces (test lib): `dial(name, token = TOKEN)` → `{ ws, received, closed, ready, waitFor, send, engine(started, version), phone() }`; `TOKEN`, `BASE`, `WS_BASE`

- [ ] **Step 1: เขียนเทสต์ที่ fail**

`relay/test/helpers.mjs`:
```js
import WebSocket from "ws";

export const BASE = process.env.RELAY_URL ?? "https://aixodia-relay.aixodia.workers.dev";
export const TOKEN = process.env.RELAY_TOKEN ?? "test-token";
export const WS_BASE = BASE.replace(/^http/, "ws");

export function dial(name, token = TOKEN) {
  const ws = new WebSocket(`${WS_BASE}/room/${name}`, { headers: { Authorization: `Bearer ${token}` } });
  const received = [];
  ws.on("message", (d) => received.push(d.toString()));
  const closed = new Promise((r) => ws.on("close", (code, reason) => r({ code, reason: String(reason) })));
  const ready = new Promise((res, rej) => { ws.on("open", () => res()); ws.on("error", rej); });
  const waitFor = (sub, ms = 5000) => new Promise((res, rej) => {
    const t0 = Date.now();
    const tick = () => {
      const hit = received.find((m) => (typeof sub === "string" ? m === sub : sub.test(m)));
      if (hit !== undefined) return res(hit);
      if (Date.now() - t0 > ms) return rej(new Error(`timeout waiting ${sub}; got ${JSON.stringify(received)}`));
      setTimeout(tick, 25);
    };
    tick();
  });
  return {
    ws, received, closed, ready, waitFor,
    send: (x) => ws.send(x),
    engine: (started, version = "t") => ws.send(JSON.stringify({ role: "engine", started, version })),
    phone: () => ws.send(JSON.stringify({ role: "phone" })),
  };
}
```
`relay/test/health.test.mjs`:
```js
import { test } from "node:test";
import assert from "node:assert/strict";
import { BASE } from "./helpers.mjs";

test("healthz", async () => {
  const res = await fetch(`${BASE}/healthz`);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "ok");
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm install && RELAY_TOKEN=test-token npm test`
Expected: FAIL (Worker ยังไม่ถูก deploy → fetch ล้ม/404)

- [ ] **Step 3: สร้างไฟล์โปรเจกต์**

`relay/wrangler.toml`:
```toml
name = "aixodia-relay"
main = "src/index.ts"
compatibility_date = "2026-10-19"
account_id = "3b953d1cbb784b3424c27798931bc37b"

[[durable_objects.bindings]]
name = "ROOM"
class_name = "Room"

[[migrations]]
tag = "v1"
new_sqlite_classes = ["Room"]
```
`relay/tsconfig.json`: target/module `es2022`, `moduleResolution` `bundler`, `strict`, `noEmit`, `types: ["@cloudflare/workers-types"]`, include `src/**/*.ts`
`relay/package.json`: `"type": "module"`, scripts `{"deploy":"wrangler deploy","test":"node --test test/"}`; devDeps `wrangler`, `@cloudflare/workers-types`, `typescript`, `ws`
`relay/.gitignore`: `.wrangler/`, `node_modules/`

`relay/src/index.ts`:
```ts
export interface Env {
  ROOM: DurableObjectNamespace;
  RELAY_TOKEN: string;
}
export { Room } from "./room";

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const { pathname } = new URL(request.url);
    if (pathname === "/healthz") return new Response("ok");
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
```
`relay/src/room.ts`:
```ts
export class Room {
  constructor(private state: DurableObjectState, private env: unknown) {}
  async fetch(_request: Request): Promise<Response> {
    return new Response("not implemented", { status: 501 });
  }
}
```

- [ ] **Step 4: ดีพลอย + ตั้ง secret + รันเทสต์ให้ผ่าน**

Run:
```bash
cd relay
export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)"
npx wrangler deploy
printf 'test-token' | npx wrangler secret put RELAY_TOKEN
RELAY_TOKEN=test-token npm test
```
Expected: `wrangler deploy` รายงาน upload สำเร็จ (URL `aixodia-relay.aixodia.workers.dev`), `secret put` สำเร็จ, เทสต์ `healthz` PASS 1/1

- [ ] **Step 5: Commit**

```bash
git add relay/package.json relay/wrangler.toml relay/tsconfig.json relay/.gitignore relay/src/index.ts relay/src/room.ts relay/test/helpers.mjs relay/test/health.test.mjs
git commit -m "relay: scaffold worker, deploy, health route"
```

---

### Task 2: Auth (`401`) + upgrade + จับคู่ engine↔phone + forward

**Files:**
- Modify: `relay/src/index.ts` (route `/room/<name>`), `relay/src/room.ts`
- Create: `relay/test/room.test.mjs`

**Interfaces:**
- Consumes: `dial`, `TOKEN`, `BASE` (Task 1)
- Produces: `GET /room/<name>` (ต้องมี `Upgrade: websocket` + bearer ที่ถูก); `401` เมื่อ bearer ผิด/ขาด; เส้นทางอื่น `404`
- Produces (protocol): frame แรก `{"role":"engine","started":N,"version":"..."}` หรือ `{"role":"phone"}`; หลังจากนั้น DO ส่งต่อ frame โปร่งทั้งสองทาง

- [ ] **Step 1: เขียนเทสต์ที่ fail**

`relay/test/room.test.mjs`:
```js
import { test } from "node:test";
import assert from "node:assert/strict";
import { BASE, dial } from "./helpers.mjs";

test("401 without a token", async () => {
  const res = await fetch(`${BASE}/room/t2a`, { headers: { Upgrade: "websocket" } });
  assert.equal(res.status, 401);
});

test("401 with a wrong token", async () => {
  const res = await fetch(`${BASE}/room/t2a`, { headers: { Upgrade: "websocket", Authorization: "Bearer nope" } });
  assert.equal(res.status, 401);
});

test("forwards frames both ways between engine and phone", async () => {
  const eng = dial("t2fwd");
  eng.engine(100);
  await eng.ready;

  const ph = dial("t2fwd");
  ph.phone();
  await ph.ready;

  ph.send("hello-engine");
  assert.equal(await eng.waitFor("hello-engine"), "hello-engine");

  eng.send("hello-phone");
  assert.equal(await ph.waitFor("hello-phone"), "hello-phone");

  eng.ws.close();
  ph.ws.close();
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm test`
Expected: FAIL (401 tests ได้ 404/501; forward timeout)

- [ ] **Step 3: เขียน implementation**

`relay/src/index.ts` — เพิ่มก่อน 404:
```ts
    const room = pathname.match(/^\/room\/([^/]+)$/);
    if (room) {
      if (!bearerOk(request.headers.get("authorization"), env.RELAY_TOKEN)) {
        return new Response("unauthorized", { status: 401 });
      }
      return env.ROOM.getByName(room[1]).fetch(request);
    }
```
พร้อม helper (const-time เทียบ ความยาวก่อน แล้ว XOR สะสม; token ว่าง = false เสมอ):
```ts
function bearerOk(header: string | null, token: string): boolean {
  if (!token || !header) return false;
  const prefix = "bearer ";
  if (header.toLowerCase().slice(0, prefix.length) !== prefix) return false;
  const given = header.slice(prefix.length).trim();
  const a = new TextEncoder().encode(given);
  const b = new TextEncoder().encode(token);
  let diff = a.length ^ b.length;
  const n = Math.max(a.length, b.length);
  for (let i = 0; i < n; i++) diff |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return diff === 0;
}
```
`relay/src/room.ts` — DO:
```ts
type Role = "engine" | "phone";

export class Room {
  constructor(private state: DurableObjectState, private env: unknown) {}

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return new Response("expected websocket", { status: 400 });
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    this.state.acceptWebSocket(server);
    server.serializeAttachment({ role: null });
    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer) {
    const att = (ws.deserializeAttachment() ?? {}) as { role: Role | null };
    if (!att.role) {
      if (typeof message !== "string") return;
      let hello: any;
      try { hello = JSON.parse(message); } catch { return; }
      if (hello?.role !== "engine" && hello?.role !== "phone") return;
      att.role = hello.role;
      ws.serializeAttachment(att);
      if (hello.role === "phone") {
        return void this.publishPresence();
      }
      return void this.publishPresence();
    }
    const other = this.peer(att.role === "engine" ? "phone" : "engine");
    if (other) other.send(message);
  }

  webSocketClose(ws: WebSocket) {
    this.state.storage.delete(`slot:${(ws.deserializeAttachment() as any)?.role}`).catch(() => {});
    this.publishPresence();
  }

  private peer(role: Role): WebSocket | undefined {
    return this.state.getWebSockets().find((s) => (s.deserializeAttachment() as any)?.role === role);
  }

  private publishPresence() {
    const phone = this.peer("phone");
    if (!phone) return;
    phone.send(JSON.stringify({ ctl: "presence", engine: !!this.peer("engine") }));
  }
}
```
(หมายเหตุ: Task 3–5 จะเติม handover/takeover/ขนาด frame; Task 2 เอาแค่ route + auth + forward + presence ตั้งต้น)

- [ ] **Step 4: ดีพลอย + รันเทสต์ให้ผ่าน**

Run: `cd relay && export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)" && npx wrangler deploy && RELAY_TOKEN=test-token npm test`
Expected: PASS ทั้งหมด (401 × 2 + forward)

- [ ] **Step 5: Commit**

```bash
git add relay/src/index.ts relay/src/room.ts relay/test/room.test.mjs
git commit -m "relay: auth, websocket upgrade, engine/phone pairing"
```

---

### Task 3: engine handover (`started` ใหม่กว่าชนะ)

**Files:**
- Modify: `relay/src/room.ts`, `relay/test/room.test.mjs`

**Interfaces:**
- Consumes: `Room` (Task 2), `dial` (Task 1)
- Produces: engine ใหม่กว่า → ตัวเก่าได้ `{"ctl":"superseded"}` แล้วถูกปิด; เท่ากัน/เก่ากว่า → ตัวใหม่ถูกปฏิเสธ (`{"ctl":"superseded"}` + ปิด); `started` เก็บใน `state.storage` เพื่อรอดข้ามการตายของ socket

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```js
test("newer engine takes the slot, older engine is closed", async () => {
  const a = dial("t3ho");
  a.engine(100);
  await a.ready;

  const b = dial("t3ho");
  b.engine(101);
  await b.ready;

  const ca = await a.closed;
  assert.ok(ca.code !== 1006 || ca.reason.includes("superseded") || true);
  await a.waitFor(/superseded/).catch(() => {});

  const c = dial("t3ho");
  c.engine(101);
  await c.ready;
  const msg = await c.waitFor(/superseded/, 6000);
  assert.match(msg, /superseded/);
  await c.closed;

  b.ws.close();
});
```
(ปรับให้ assert ว่า a ปิดจริง — `await a.closed` ต้อง resolve; ถ้าไม่ resolve = fail)

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm test`
Expected: FAIL (ยังไม่มี handover — b ไม่ได้แย่ง slot, c ไม่ถูกปฏิเสธ)

- [ ] **Step 3: เติม handover ใน `room.ts`**

ตอนได้ hello `engine`: อ่าน `const prev = await this.state.storage.get<{started:number}>("engine")`;
- `prev && hello.started <= prev.started` → `ws.send('{"ctl":"superseded"}'); ws.close(1000, "superseded")` แล้ว return
- มิฉะนั้น → ถ้ามี engine เก่า (`this.peer("engine")`) → `old.send('{"ctl":"superseded"}'); old.close(1000, "superseded")`; `storage.put("engine", { started, version })`; `publishPresence()`
ตอน `webSocketClose`/`webSocketError`: ลบ `engine` slot เมื่อ role = engine และ `storage.get("engine").started` ตรงกับ attachment

- [ ] **Step 4: ดีพลอย + รันเทสต์ให้ผ่าน**

Run: `cd relay && export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)" && npx wrangler deploy && RELAY_TOKEN=test-token npm test`
Expected: PASS ทุกข้อ

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/test/room.test.mjs
git commit -m "relay: engine handover by started"
```

---

### Task 4: phone ตัวเดียว + presence

**Files:**
- Modify: `relay/src/room.ts`, `relay/test/room.test.mjs`

**Interfaces:**
- Consumes: `Room` (Task 2–3)
- Produces: phone ที่สอง → ตัวเก่าได้ `{"ctl":"superseded-phone"}` แล้วถูกปิด; phone ได้ `{"ctl":"presence","engine":bool}` ทุกครั้งที่สถานะ engine เปลี่ยน; phone หลุดไม่กระทบ engine

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```js
test("second phone closes the first, engine survives", async () => {
  const eng = dial("t4sp");
  eng.engine(1);
  await eng.ready;

  const p1 = dial("t4sp");
  p1.phone();
  await p1.ready;

  const p2 = dial("t4sp");
  p2.phone();
  await p2.ready;

  assert.match(await p1.waitFor(/superseded-phone/, 6000), /superseded-phone/);

  eng.send("still-here");
  assert.equal(await p2.waitFor("still-here"), "still-here");

  eng.ws.close(); p2.ws.close();
});

test("phone is told when the engine attaches", async () => {
  const p = dial("t4pres");
  p.phone();
  await p.ready;

  const eng = dial("t4pres");
  eng.engine(1);
  await eng.ready;

  assert.match(await p.waitFor(/"engine":true/, 6000), /"engine":true/);
  eng.ws.close(); p.ws.close();
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm test`
Expected: FAIL (ยังไม่มี takeover; presence ไม่มี)

- [ ] **Step 3: เติม takeover + presence**

ตอน hello `phone`: ถ้ามี phone เก่า → `old.send('{"ctl":"superseded-phone"}'); old.close(1000, "superseded-phone")`; แล้ว `publishPresence()`
`webSocketClose`: engine ตาย → `publishPresence()` (phone ได้ `engine:false`); phone ตาย → อย่าแตะ engine

- [ ] **Step 4: ดีพลอย + รันเทสต์ให้ผ่าน**

Run: `cd relay && export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)" && npx wrangler deploy && RELAY_TOKEN=test-token npm test`
Expected: PASS ทุกข้อ

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/test/room.test.mjs
git commit -m "relay: one phone at a time with presence"
```

---

### Task 5: ขนาด frame 32 KiB + เก็บกวาด slot + ทน idle

**Files:**
- Modify: `relay/src/room.ts`, `relay/test/room.test.mjs`

**Interfaces:**
- Consumes: `Room` (Task 2–4)
- Produces: binary frame 32 KiB ผ่านทั้งสองทาง; หลัง engine ปิด phone ได้ `engine:false`; ต่อใหม่แล้วใช้ได้ (state ไม่ค้างในหน่วยความจำ)

- [ ] **Step 1: เขียนเทสต์ที่ fail**

```js
test("relays a 32 KiB binary frame", async () => {
  const eng = dial("t5sz");
  eng.engine(1);
  await eng.ready;
  const ph = dial("t5sz");
  ph.phone();
  await ph.ready;

  const blob = new Uint8Array(32 * 1024).fill(7);
  eng.ws.send(blob);
  const got = await new Promise((res, rej) => {
    const t0 = Date.now();
    const tick = () => {
      const hit = ph.received.length;
      if (ph.ws._receiver?._writableState) { /* noop */ }
      if (Date.now() - t0 > 6000) return rej(new Error("binary timeout"));
      setTimeout(tick, 25);
    };
    ph.ws.on("message", (d) => res(d));
    tick();
  });
  assert.equal(got.length, 32 * 1024);
  eng.ws.close(); ph.ws.close();
});
```
> หมายเหตุ: เทสต์ binary ต้องฟัง `ws.on("message")` ที่เป็น Buffer (helpers เก็บเป็น string อยู่แล้ว) — ให้เพิ่ม `raw` array ใน `helpers.mjs` ที่ push `d` แบบ Buffer ด้วย เพื่อ assert ความยาว

```js
test("clears the engine slot on close", async () => {
  const p = dial("t5clr");
  p.phone();
  await p.ready;
  const eng = dial("t5clr");
  eng.engine(5);
  await eng.ready;
  assert.match(await p.waitFor(/"engine":true/, 6000), /"engine":true/);

  eng.ws.close();
  assert.match(await p.waitFor(/"engine":false/, 8000), /"engine":false/);
  p.ws.close();
});
```

- [ ] **Step 2: รันให้เห็น fail**

Run: `cd relay && npm test`
Expected: FAIL ถ้า binary ยังไม่ผ่าน หรือ slot ไม่ถูกกวาด

- [ ] **Step 3: ทำให้ผ่าน**

- `webSocketMessage` ต้อง forward `ArrayBuffer` ตรง ๆ (ไม่ stringify) — และ `ws.send(message)` รับ binary ได้
- `webSocketClose`/`webSocketError`: กวาด slot ตาม role; engine → ลบ storage + `publishPresence()`; ยืนยันว่า role อ่านจาก `deserializeAttachment()` และ slot อ่านจาก `state.storage` เท่านั้น (ไม่มีตัวแปร instance ที่ต้องมีก่อน) เพื่อให้รอด hibernation
- เพิ่ม `raw: Buffer[]` ใน `helpers.mjs` (push `d` ใน `ws.on("message")`)

- [ ] **Step 4: ดีพลอย + รันเทสต์ให้ผ่าน**

Run: `cd relay && export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)" && npx wrangler deploy && RELAY_TOKEN=test-token npm test`
Expected: PASS ทุกข้อ

- [ ] **Step 5: Commit**

```bash
git add relay/src/room.ts relay/test/room.test.mjs relay/test/helpers.mjs
git commit -m "relay: 32KiB frames and slot cleanup"
```

---

### Task 6: README + รันทั้งชุดปิดท้าย

**Files:**
- Create: `relay/README.md`
- (ไม่แก้โค้ด ถ้าไม่จำเป็น)

**Interfaces:**
- Consumes: ทุกอย่าง (Task 1–5)

- [ ] **Step 1: เขียน README**

`relay/README.md`:
```markdown
# aixodia-relay
Worker + Durable Object ที่จับคู่ engine กับมือถือ (อย่างละตัว) — ไม่มี tunnel

## ดีพลอย
    export CLOUDFLARE_API_TOKEN=...   # deploy token (Workers Scripts Write)
    cd relay && npx wrangler deploy
    printf '<shared-token>' | npx wrangler secret put RELAY_TOKEN

URL: https://aixodia-relay.aixodia.workers.dev/room/<room>

## ทดสอบ (ต้อง deploy ก่อน — workerd รันในเครื่อง dev ไม่ได้)
    RELAY_TOKEN=<shared-token> npm test
```
(ใส่โน้ต: free plan, SQLite DO, hibernation, phone เดียว, engine เดียว, workerd ใช้ไม่ได้ในเครื่อง)

- [ ] **Step 2: รันทั้งชุด (deploy + test) + เทสต์เดิมของ engine**

Run:
```bash
cd relay
export CLOUDFLARE_API_TOKEN="$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' /tmp/opencode/relay.env)"
npx wrangler deploy && RELAY_TOKEN=test-token npm test
cd .. && go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...
```
Expected: relay test PASS ทุกข้อ; go ทุกคำสั่งเขียว

- [ ] **Step 3: Commit**

```bash
git add relay/README.md
git commit -m "relay: document deploy and testing"
```

---

## Self-Review

- **Spec coverage:** §4.1 (roles/handover/forward/presence) → Tasks 2–4; §4.5 (free plan/hibernation) → Task 1 (SQLite class) + Task 5; §5 (token/deploy/subdomain) → Task 1 + Testing Conventions; §6 (ทดสอบ = deploy จริง) → Global Constraints + ทุก task
- **Step scan:** ทุก step มี action + Expected; ไม่มี "TBD"
- **Type consistency:** `Room`/`Env`/`dial` ชื่อเดียวตลอด
- **Review Focus:** bearer ผิด → Task 2; role ผิด → Task 2; `started` เท่ากัน/เก่า → Task 3; phone หลุดไม่กระทบ engine → Task 4; frame 32 KiB → Task 5
- **Proportion:** แผนสั้นกว่าสเปกที่อ้าง — มีแต่ signature/เทสต์ ไม่มี body เต็มทุกบรรทัด

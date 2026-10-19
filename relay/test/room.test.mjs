import { test } from "node:test";
import assert from "node:assert/strict";
import { BASE, dial, timed } from "./helpers.mjs";

test("401 without a token", async () => {
  const res = await fetch(`${BASE}/room/t2a`);
  assert.equal(res.status, 401);
});

test("401 with a wrong token", async () => {
  const res = await fetch(`${BASE}/room/t2a`, { headers: { Authorization: "Bearer nope" } });
  assert.equal(res.status, 401);
});

test("forwards frames both ways between engine and phone", async () => {
  const eng = await dial("t2fwd");
  await eng.engine(100);

  const ph = await dial("t2fwd");
  await ph.phone();

  await ph.send("hello-engine");
  assert.equal(await eng.waitFor("hello-engine"), "hello-engine");

  await eng.send("hello-phone");
  assert.equal(await ph.waitFor("hello-phone"), "hello-phone");

  eng.ws.close();
  ph.ws.close();
});

test("newer engine takes the slot, older engine is closed", async () => {
  const a = await dial("t3ho");
  await a.engine(100, "a");

  const b = await dial("t3ho");
  await b.engine(101, "b");

  assert.match(await a.waitFor(/superseded/, 8000), /superseded/);
  const ca = await timed(a.closed, 8000, "the old engine must be closed");
  assert.notEqual(ca.code, 1006);

  const c = await dial("t3ho");
  await c.engine(101, "c");
  assert.match(await c.waitFor(/superseded/, 8000), /superseded/);
  await timed(c.closed, 8000, "a stale-started engine must be closed");

  b.ws.close();
});

test("an engine with an older started never takes the slot", async () => {
  const a = await dial("t3old");
  await a.engine(500, "a");

  const b = await dial("t3old");
  await b.engine(400, "b");
  assert.match(await b.waitFor(/superseded/, 8000), /superseded/);
  await timed(b.closed, 8000, "the older engine must be closed");

  const still = await Promise.race([
    a.closed.then(() => "closed"),
    new Promise((r) => setTimeout(() => r("open"), 1500)),
  ]);
  assert.equal(still, "open");

  a.ws.close();
});

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

test("the second phone closes the first, and the engine survives", async () => {
  const eng = await dial("t4sp");
  await eng.engine(1);

  const p1 = await dial("t4sp");
  await p1.phone();

  const p2 = await dial("t4sp");
  await p2.phone();

  assert.match(await p1.waitFor(/superseded-phone/, 8000), /superseded-phone/);
  await timed(p1.closed, 8000, "the first phone must be closed");

  await eng.send("still-here");
  assert.equal(await p2.waitFor("still-here"), "still-here");

  eng.ws.close();
  p2.ws.close();
});

test("the phone is told when the engine attaches and when it leaves", async () => {
  const p = await dial("t4pres");
  await p.phone();

  const eng = await dial("t4pres");
  await eng.engine(1);
  assert.match(await p.waitFor(/"engine":true/, 8000), /"engine":true/);

  eng.ws.close();
  assert.match(await p.waitFor(/"engine":false/, 8000), /"engine":false/);

  p.ws.close();
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { BASE, dial } from "./helpers.mjs";

test("401 without a token", async () => {
  const res = await fetch(`${BASE}/room/t2a`);
  assert.equal(res.status, 401);
});

test("401 with a wrong token", async () => {
  const res = await fetch(`${BASE}/room/t2a`, { headers: { Authorization: "Bearer nope" } });
  assert.equal(res.status, 401);
});

test("forwards frames both ways between engine and phone", async () => {
  const eng = dial("t2fwd");
  await eng.ready;
  await eng.engine(100);

  const ph = dial("t2fwd");
  await ph.ready;
  await ph.phone();

  await ph.send("hello-engine");
  assert.equal(await eng.waitFor("hello-engine"), "hello-engine");

  await eng.send("hello-phone");
  assert.equal(await ph.waitFor("hello-phone"), "hello-phone");

  eng.ws.close();
  ph.ws.close();
});

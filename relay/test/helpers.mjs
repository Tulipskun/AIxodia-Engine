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
  const send = (x) => ready.then(() => ws.send(x));
  return {
    ws, received, closed, ready, waitFor, send,
    engine: (started, version = "t") => send(JSON.stringify({ role: "engine", started, version })),
    phone: () => send(JSON.stringify({ role: "phone" })),
  };
}

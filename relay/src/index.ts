export interface Env {
  ROOM: DurableObjectNamespace;
  RELAY_TOKEN: string;
}
export { Room2 } from "./room";

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

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const { pathname } = new URL(request.url);
    if (pathname === "/healthz") return new Response("ok");

    const room = pathname.match(/^\/room\/([^/]+)$/);
    if (room) {
      if (!bearerOk(request.headers.get("authorization"), env.RELAY_TOKEN)) {
        return new Response("unauthorized", { status: 401 });
      }
      return env.ROOM.getByName(room[1]).fetch(request);
    }

    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;

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

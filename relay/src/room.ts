type Role = "engine" | "phone";

type Attachment = { role: Role | null };

export class Room {
  constructor(private state: DurableObjectState, private env: unknown) {}

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return new Response("expected websocket", { status: 400 });
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair) as [WebSocket, WebSocket];
    this.state.acceptWebSocket(server);
    server.serializeAttachment({ role: null } satisfies Attachment);
    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer) {
    const att = (ws.deserializeAttachment() ?? { role: null }) as Attachment;
    if (!att.role) {
      if (typeof message !== "string") return;
      let hello: unknown;
      try {
        hello = JSON.parse(message);
      } catch {
        return;
      }
      const role = (hello as { role?: unknown } | null)?.role;
      if (role !== "engine" && role !== "phone") return;
      att.role = role;
      ws.serializeAttachment(att);
      this.publishPresence();
      return;
    }
    const other = this.peer(att.role === "engine" ? "phone" : "engine");
    if (other) other.send(message);
  }

  webSocketClose(_ws: WebSocket) {
    this.publishPresence();
  }

  webSocketError(_ws: WebSocket) {
    this.publishPresence();
  }

  private peer(role: Role): WebSocket | undefined {
    return this.state
      .getWebSockets()
      .find((s) => ((s.deserializeAttachment() ?? { role: null }) as Attachment).role === role);
  }

  private publishPresence() {
    const phone = this.peer("phone");
    if (!phone) return;
    phone.send(JSON.stringify({ ctl: "presence", engine: !!this.peer("engine") }));
  }
}

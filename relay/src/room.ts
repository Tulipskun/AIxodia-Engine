type Role = "engine" | "phone";

type Attachment = { role: Role | null; started?: number };

type EngineSlot = { started: number; version: string };

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

      if (role === "engine") {
        const started = Number((hello as { started?: unknown }).started);
        if (!Number.isFinite(started)) return;
        const prev = await this.state.storage.get<EngineSlot>("engine");
        if (prev && started <= prev.started) {
          this.reject(ws, "superseded");
          return;
        }
        const old = this.peer("engine");
        if (old && old !== ws) this.reject(old, "superseded");
        att.role = "engine";
        att.started = started;
        ws.serializeAttachment(att);
        await this.state.storage.put("engine", {
          started,
          version: String((hello as { version?: unknown }).version ?? ""),
        } satisfies EngineSlot);
        this.publishPresence();
        return;
      }

      if (role === "phone") {
        att.role = "phone";
        ws.serializeAttachment(att);
        this.publishPresence();
        return;
      }
      return;
    }

    const other = this.peer(att.role === "engine" ? "phone" : "engine");
    if (other) other.send(message);
  }

  async webSocketClose(ws: WebSocket) {
    await this.clear(ws);
  }

  async webSocketError(ws: WebSocket) {
    await this.clear(ws);
  }

  private async clear(ws: WebSocket) {
    const att = (ws.deserializeAttachment() ?? { role: null }) as Attachment;
    if (att.role === "engine") {
      const slot = await this.state.storage.get<EngineSlot>("engine");
      if (slot && slot.started === att.started) await this.state.storage.delete("engine");
    }
    this.publishPresence();
  }

  private reject(ws: WebSocket, ctl: string) {
    ws.send(JSON.stringify({ ctl }));
    ws.close(1000, ctl);
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

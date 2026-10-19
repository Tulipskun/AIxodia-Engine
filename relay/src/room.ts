export class Room {
  constructor(private state: DurableObjectState, private env: unknown) {}

  async fetch(_request: Request): Promise<Response> {
    return new Response("not implemented", { status: 501 });
  }
}

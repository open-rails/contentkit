import { expect, it, vi } from "vitest";
import { readProgress } from "./progress.js";

it("parses split CRLF frames, ignores comments, and releases its reader on abort", async () => {
  const ctl = new AbortController();
  const cancel = vi.fn();
  const chunks = [": comment\r\nid: c1\r\nevent: snap", "shot\r\ndata: {\"files\":[],\r\ndata: \"status\":{\"state\":\"processing\"}}\r\n\r\n"];
  const body = new ReadableStream<Uint8Array>({ start(reader) { for (const chunk of chunks) reader.enqueue(new TextEncoder().encode(chunk)); }, cancel });
  const receive = vi.fn(() => { ctl.abort(); });
  await expect(readProgress(new Response(body, { headers: { "Content-Type": "text/event-stream" } }), receive, ctl.signal)).rejects.toMatchObject({ code: "aborted" });
  expect(receive).toHaveBeenCalledWith({ event: "snapshot", cursor: "c1", read: { files: [], status: { state: "processing" } } });
  expect(cancel).toHaveBeenCalledOnce();
});

it("keeps auth refusals terminal and cancels oversized events", async () => {
  for (const [data, code] of [["event: error\ndata: {\"code\":\"unauthorized\",\"error\":\"expired\"}\n\n", "unauthorized"], ["data: " + "x".repeat(1024 * 1024 + 1), "invalid_request"]]) {
    const canceled = vi.fn();
    const body = new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(data)); }, cancel: canceled });
    await expect(readProgress(new Response(body, { headers: { "Content-Type": "text/event-stream" } }), () => {}, new AbortController().signal)).rejects.toMatchObject({ code });
    expect(canceled).toHaveBeenCalledOnce();
  }
});

it("waits for processing events before rereading an upload and closes the stream on completion", async () => {
  const { createContentKitClient } = await import("../client.js");
  let reads = 0;
  let source!: ReadableStreamDefaultController<Uint8Array>;
  const cancel = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: async (_input, init) => {
    if (new Headers(init?.headers).get("Accept") === "text/event-stream") {
      return new Response(new ReadableStream<Uint8Array>({ start(controller) { source = controller; }, cancel }), { headers: { "Content-Type": "text/event-stream" } });
    }
    reads++;
    return Response.json({ files: [{ path: "originals/a.png", upload: true, size: 12, ...(reads < 3 ? { pending: ["thumb"] } : {}) }] });
  } });
  const emit = (event: string) => source.enqueue(new TextEncoder().encode(`id: next\nevent: ${event}\ndata: {"files":[],"status":{"state":"processing"}}\n\n`));
  const result = client.media.waitFor({ kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" }, "originals/a.png");
  await vi.waitFor(() => expect(source).toBeDefined());
  expect(reads).toBe(1);
  emit("snapshot");
  await vi.waitFor(() => expect(reads).toBe(2));
  emit("progress");
  await expect(result).resolves.toMatchObject({ path: "originals/a.png", size: 12 });
  expect(reads).toBe(3);
  expect(cancel).toHaveBeenCalledOnce();
});

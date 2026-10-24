// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useMediaRead } from "./read.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };
const status = (done = 5) => ({ access: "none", files: [], expires: 0, total: 0, offset: 0, limit: 0, state: done === 30 ? "ready" : "processing", status: {
  state: done === 30 ? "ready" : "processing", failure_count: 0, failures: [],
  uploads: { completed: 1, total: 1, failed: 0 }, archives: { completed: 0, total: 1, failed: 0, pages: { completed: done, total: 30, failed: 0 } }, presets: [],
} });
afterEach(() => { cleanup(); vi.useRealTimers(); });

function transport() {
  const requests: { url: URL; init?: RequestInit }[] = [];
  const streams: { send: (event: string, cursor: string, done?: number) => void; close: () => void; canceled: ReturnType<typeof vi.fn> }[] = [];
  const fetch: typeof globalThis.fetch = async (input, init) => {
    requests.push({ url: new URL(String(input)), init });
    if (new Headers(init?.headers).get("Accept") !== "text/event-stream") return Response.json(status());
    const canceled = vi.fn();
    let source!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({ start(controller) { source = controller; }, cancel: canceled });
    streams.push({ canceled, close: () => source.close(), send: (event, cursor, done = 5) => source.enqueue(new TextEncoder().encode(`id: ${cursor}\nevent: ${event}\ndata: ${JSON.stringify(status(done))}\n\n`)) });
    return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
  };
  return { requests, streams, fetch };
}

it("shares one authenticated stream, updates counts, and stops on completion without periodic reads", async () => {
  const t = transport();
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: t.fetch, token: () => "credential", headers: () => ({ "X-CSRF": "csrf" }) });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="owner">{children}</ContentKitProvider>;
  const { result } = renderHook(() => [useMediaRead(ref, { status: true, editor: true }), useMediaRead(ref, { status: true, editor: true })], { wrapper });
  await waitFor(() => expect(t.streams).toHaveLength(1));
  expect(t.requests).toHaveLength(2);
  const headers = new Headers(t.requests[1]!.init?.headers);
  expect(headers.get("Authorization")).toBe("Bearer credential");
  expect(headers.get("X-CSRF")).toBe("csrf");
  await act(async () => t.streams[0]!.send("snapshot", "one", 5));
  expect(result.current[0]?.read?.status?.archives.pages?.completed).toBe(5);
  await act(async () => t.streams[0]!.send("progress", "two", 12));
  expect(result.current[1]?.read?.status?.archives.pages?.completed).toBe(12);
  await act(async () => t.streams[0]!.send("progress", "three", 30));
  expect(result.current[0]?.processing).toBe(false);
  expect(t.streams[0]!.canceled).toHaveBeenCalledOnce();
  expect(t.requests).toHaveLength(2);
});

it("cancels on scope/ref switch and unmount, and reopened pages recover persisted progress", async () => {
  const t = transport();
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: t.fetch });
  let viewer = "alice";
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const hook = renderHook(({ id }) => useMediaRead({ ...ref, id }, { status: true, editor: true }), { wrapper, initialProps: { id: ref.id } });
  await waitFor(() => expect(t.streams).toHaveLength(1));
  viewer = "bob";
  hook.rerender({ id: ref.id });
  await waitFor(() => expect(t.streams).toHaveLength(2));
  expect(t.streams[0]!.canceled).toHaveBeenCalledOnce();
  hook.rerender({ id: "0192f000-0000-7000-8000-000000000002" });
  await waitFor(() => expect(t.streams).toHaveLength(3));
  expect(t.streams[1]!.canceled).toHaveBeenCalledOnce();
  hook.unmount();
  await waitFor(() => expect(t.streams[2]!.canceled).toHaveBeenCalledOnce());
  const reopened = renderHook(() => useMediaRead(ref, { status: true, editor: true }), { wrapper });
  await waitFor(() => expect(t.streams).toHaveLength(4));
  await act(async () => t.streams[3]!.send("snapshot", "fresh", 23));
  expect(reopened.result.current.read?.status?.archives.pages?.completed).toBe(23);
});

it("resumes a disconnected cursor, applies resync, and exposes exhausted reconnects", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const t = transport();
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: t.fetch });
  const { result } = renderHook(() => useMediaRead(ref, { status: true, editor: true, client }));
  const flush = async () => act(async () => { await vi.advanceTimersByTimeAsync(1); });
  await flush();
  await act(async () => t.streams[0]!.send("snapshot", "saved", 5));
  await act(async () => t.streams[0]!.close());
  await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
  expect(t.requests.at(-1)?.url.searchParams.get("after")).toBe("saved");
  await act(async () => t.streams[1]!.send("resync", "new", 19));
  expect(result.current.read?.status?.archives.pages?.completed).toBe(19);
  for (const delay of [2000, 4000]) {
    await act(async () => t.streams.at(-1)!.close());
    await act(async () => { await vi.advanceTimersByTimeAsync(delay); });
  }
  await act(async () => t.streams.at(-1)!.close());
  expect(t.streams).toHaveLength(4);
  expect(result.current.error?.code).toBe("network");
  expect(result.current.read).toBeNull();
});

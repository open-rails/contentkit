import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";
import type { ContentKitChange } from "./http.js";

const ref = { kind: "video", id: "0192f000-0000-7000-8000-000000000001" };
const read = { access: "full", expires: 0, total: 0, offset: 0, limit: 50, files: [] };

it("serves every module under baseUrl unless a mount moves it", () => {
  const one = createContentKitClient({ baseUrl: "/api/contentkit/" });
  expect(["content", "media", "upload", "codes", "entities", "kinds", "contents"].map((m) => one.url(m as never))).toEqual([
    "/api/contentkit/v1",
    "/api/contentkit/v1/media",
    "/api/contentkit/v1/media/upload",
    "/api/contentkit/v1/codes",
    "/api/contentkit/v1",
    "/api/contentkit/v1",
    "/api/contentkit/v1",
  ]);
  const split = createContentKitClient({
    baseUrl: "/api/v1/social",
    mounts: { media: "/api/v1/media/", upload: (r) => (r.kind === "video" ? "/api/v1/media-uploads" : "/api/v1/user-uploads") },
  });
  expect(split.url("upload", ref)).toBe("/api/v1/media-uploads");
  expect(split.url("upload", { ...ref, kind: "avatar" })).toBe("/api/v1/user-uploads");
  expect(split.media.hlsBase(ref, "hls/")).toBe(`/api/v1/media/video/${ref.id}/hls/hls/`);
  expect(() => split.url("upload")).toThrow(/per item/);
});

it("sends the token and language, and reports commits to subscribers", async () => {
  const seen: { url: string; headers: Headers }[] = [];
  const client = createContentKitClient({
    baseUrl: "http://api.test/ck",
    token: async () => "tok",
    language: () => "ja",
    fetch: (async (url: string, init: RequestInit) => {
      seen.push({ url, headers: new Headers(init.headers) });
      return new Response(JSON.stringify(url.endsWith("/commit") ? { files: [{ path: "a.png", type: "image/png", upload: true }] } : read), { status: 200 });
    }) as typeof fetch,
  });
  const changes: ContentKitChange[] = [];
  const off = client.subscribe((c) => changes.push(c));
  await client.media.read(ref, { editor: true });
  await client.media.commit(ref, [{ op: "remove", path: "b.png" }]);
  off();
  await client.media.commit(ref, [{ op: "remove", path: "c.png" }]);
  expect(seen.map((s) => s.url)).toEqual([`http://api.test/ck/v1/media/video/${ref.id}?editor=1`, "http://api.test/ck/v1/media/upload/commit", "http://api.test/ck/v1/media/upload/commit"]);
  expect([seen[0]!.headers.get("Authorization"), seen[0]!.headers.get("Accept-Language")]).toEqual(["Bearer tok", "ja"]);
  expect(changes).toEqual([{ type: "media.committed", ref, files: [{ path: "a.png", type: "image/png", upload: true }] }]);
});

it("HLS loader: API playlists go through the client's fetch, segments carry only cookies", async () => {
  const calls: { url: string; auth: string | null; credentials?: RequestCredentials }[] = [];
  const record = (url: string, init?: RequestInit) => {
    calls.push({ url, auth: new Headers(init?.headers).get("Authorization"), credentials: init?.credentials });
    return Promise.resolve(new Response("#EXTM3U"));
  };
  const plain = globalThis.fetch;
  globalThis.fetch = ((url: string, init?: RequestInit) => record(`plain ${url}`, init)) as typeof fetch;
  try {
    const client = createContentKitClient({ baseUrl: "https://api.test/ck", token: () => "tok", credentials: "include", fetch: ((url: string, init?: RequestInit) => record(url, init)) as typeof fetch });
    await client.media.hls.playlist("https://api.test/ck/media/video/x/hls/master.m3u8");
    await client.media.hls.playlist("https://media.test/x/master.m3u8");
    expect(calls).toEqual([
      { url: "https://api.test/ck/media/video/x/hls/master.m3u8", auth: "Bearer tok", credentials: "include" },
      { url: "plain https://media.test/x/master.m3u8", auth: null, credentials: "include" },
    ]);
  } finally {
    globalThis.fetch = plain;
  }
  const client = createContentKitClient({ baseUrl: "https://api.test/ck", token: () => "tok", credentials: "include" });
  const x = { readyState: 0, withCredentials: false, opened: "", headers: {} as Record<string, string> };
  const xhr = Object.assign(x, {
    open: (_m: string, url: string) => ((x.opened = url), (x.readyState = 1)),
    setRequestHeader: (k: string, v: string) => (x.headers[k] = v),
  });
  client.media.hls.segment(xhr as unknown as XMLHttpRequest, "https://api.test/ck/media/video/x/seg/0.m4s");
  expect([x.opened, x.withCredentials, x.headers]).toEqual(["https://api.test/ck/media/video/x/seg/0.m4s", true, {}]);
});


it("reads the authenticated inbox and returns canonical local receipts", async () => {
  const calls: { url: string; method: string | undefined }[] = [];
  const client = createContentKitClient({
    baseUrl: "http://api.test/ck",
    fetch: (async (url: string, init: RequestInit) => {
      calls.push({ url, method: init.method });
      return Response.json(init.method === "GET" ? { items: [], next_cursor: "next" } : { id: "item/1", read_at: init.method === "PUT" ? "2026-10-11T12:00:00Z" : null, revision: 2 });
    }) as typeof fetch,
  });
  const changes: ContentKitChange[] = [];
  client.subscribe((c) => changes.push(c));
  expect(await client.notifications.list({ limit: 10, cursor: "opaque", unread: true })).toEqual({ items: [], next_cursor: "next" });
  expect((await client.notifications.setRead("item/1")).revision).toBe(2);
  await client.notifications.setRead("item/1", false);
  expect(calls).toEqual([
    { url: "http://api.test/ck/v1/notifications/inbox?limit=10&cursor=opaque&unread=true", method: "GET" },
    { url: "http://api.test/ck/v1/notifications/item%2F1/read", method: "PUT" },
    { url: "http://api.test/ck/v1/notifications/item%2F1/read", method: "DELETE" },
  ]);
  expect(changes).toEqual([
    { type: "notifications.updated", item: { id: "item/1", read_at: "2026-10-11T12:00:00Z", revision: 2 } },
    { type: "notifications.updated", item: { id: "item/1", read_at: null, revision: 2 } },
  ]);
});

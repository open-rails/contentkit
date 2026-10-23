import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";
import type { ContentKitChange } from "./http.js";

it("pulls one event page without losing IDs or creating a reader loop", async () => {
  const calls: URL[] = [];
  const page = {
    events: [{ id: "9007199254740993", version: 1, type: "content.updated", kind: "gallery", content_id: "item", occurred_at: "2026-10-23T12:00:00Z" }],
    next_cursor: "continue", has_more: false, retention_seconds: 7_776_000,
  };
  const client = createContentKitClient({
    baseUrl: "https://example.test/contentkit",
    fetch: (async (url: string) => { calls.push(new URL(url)); return Response.json(page); }) as typeof fetch,
  });
  const changes: ContentKitChange[] = [];
  client.subscribe((change) => changes.push(change));
  expect(await client.catalog.listEvents({ after: "opaque", limit: 20, types: ["content.updated", "content.purged"] })).toEqual(page);
  expect(calls).toHaveLength(1);
  expect(calls[0]!.pathname).toBe("/contentkit/v1/events");
  expect(calls[0]!.searchParams.get("after")).toBe("opaque");
  expect(calls[0]!.searchParams.get("limit")).toBe("20");
  expect(calls[0]!.searchParams.get("types")).toBe("content.updated,content.purged");
  expect(changes).toEqual([]);
});

it("exposes an expired cursor instead of silently restarting history", async () => {
  let calls = 0;
  const client = createContentKitClient({
    baseUrl: "https://example.test/contentkit",
    fetch: (async () => { calls++; return Response.json({ code: "event_cursor_expired", error: "Resync current state." }, { status: 410 }); }) as typeof fetch,
  });
  await expect(client.catalog.listEvents({ after: "expired" })).rejects.toMatchObject({ code: "event_cursor_expired", status: 410 });
  expect(calls).toBe(1);
});

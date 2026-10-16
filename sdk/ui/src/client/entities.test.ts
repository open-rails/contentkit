import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";
import type { ContentKitChange } from "./http.js";

it("uses entity routes for the directory, content metadata and interactions", async () => {
  const paths: string[] = [];
  const client = createContentKitClient({
    baseUrl: "/ck",
    fetch: (async (input: string) => {
      paths.push(input);
      return Response.json({ entities: [], memberships: [], count: 0 });
    }) as typeof fetch,
  });
  await client.entities.list({ kind: "artist" });
  await client.entities.ofContent("gallery-id");
  await client.comments.list({ type: "entities", id: "artist-id" });
  await client.favorites.set({ type: "entities", id: "artist-id" }, true);
  await client.reactions.get({ type: "entities", id: "artist-id" });
  expect(paths).toEqual([
    "/ck/v1/entities?kind=artist",
    "/ck/v1/contents/gallery-id/entities",
    "/ck/v1/entities/artist-id/comments",
    "/ck/v1/entities/artist-id/favorite",
    "/ck/v1/entities/artist-id/reaction",
  ]);
});

it("uses the entity mount and invalidates entity reads after a write", async () => {
  const paths: string[] = [];
  const changes: ContentKitChange[] = [];
  const client = createContentKitClient({
    baseUrl: "/ck",
    mounts: { entities: "https://metadata.example/v1/" },
    fetch: (async (input: string) => {
      paths.push(input);
      return Response.json([{ id: "artist-id" }]);
    }) as typeof fetch,
  });
  client.subscribe((change) => changes.push(change));
  await client.entities.create([{ kind: "artist", slug: "artist" }]);
  expect(paths).toEqual(["https://metadata.example/v1/entities"]);
  expect(changes).toEqual([{ type: "entities.changed" }]);
});

it("puts an optional management kind in the route and keeps other filters in the query", async () => {
  const urls: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://content.example/ck",
    fetch: (async (input: string) => {
      urls.push(new URL(input));
      return Response.json([]);
    }) as typeof fetch,
  });
  await client.contents.manage({ q: "winter", limit: 10 });
  await client.contents.manage({ kind: "gallery", q: "winter", state: "draft", language: "en" });
  expect(urls.map((url) => url.pathname)).toEqual(["/ck/v1/manage", "/ck/v1/manage/gallery"]);
  expect(urls.map((url) => url.searchParams.has("kind"))).toEqual([false, false]);
  expect(Object.fromEntries(urls[1]!.searchParams)).toEqual({ q: "winter", state: "draft", language: "en" });
});

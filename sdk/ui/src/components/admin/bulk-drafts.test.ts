import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, UploadRule } from "../../client/generated/wire.js";
import { applyDraftDefaults, createDraft, fileTitle, uploadTarget } from "./bulk-drafts.js";

const id = "01a14382-2b60-7019-a80a-3e24e24491c2";
const v1 = "01a14382-2b61-7019-a80a-3e24e24491c3";
const v2 = "01a14382-2b62-7019-a80a-3e24e24491c4";
const rules: UploadRule[] = [
  { path: "cover", types: ["image/png", "image/jpeg"], max_bytes: 0, max: 1 },
  { path: "covers/{name}", types: ["image/png", "image/jpeg"], max_bytes: 0 },
  { path: "originals/{name}", types: ["image/png", "image/jpeg"], max_bytes: 0, page: true },
  { path: "import/{name}", types: ["application/zip"], max_bytes: 0 },
];

const model: ContentModel = { languages: ["en", "ja"], tiers: ["premium"], signals: false, kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" } } }, entity_kinds: { artist: { route: "artists" } } };

function server(uploads: UploadRule[] | undefined, fail?: string) {
  const posts: unknown[] = [];
  const writes: { method: string; path: string; body: unknown }[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (method !== "GET") writes.push({ method, path: url.pathname.replace(/^\/v1/, ""), body });
    if (init?.method === "POST" && url.pathname.endsWith("/contents")) {
      posts.push(body);
      const created = { id: id.replace(/.$/, String(posts.length + 1)), kind: "gallery", attributes: {}, versions: [{ id: v2, position: 2 }, { id: v1, position: 1 }] } as unknown as Content;
      return Response.json(created, { status: 201 });
    }
    if (method === "PATCH" || method === "PUT") {
      if (fail && url.pathname.includes(fail)) return Response.json({ code: "invalid_request", error: "Choose another artist and retry." }, { status: 400 });
      return Response.json({ id: url.pathname.split("/").pop(), kind: "gallery", attributes: {}, ...body });
    }
    if (url.pathname.includes("/media/gallery/")) return Response.json({ files: [], ...(uploads ? { uploads } : {}) });
    return Response.json({ code: "not_found", error: url.pathname }, { status: 404 });
  });
  return { client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }), posts, writes };
}

it("titles a draft from its file name and uploads into its first version", async () => {
  expect(fileTitle("My_Gallery-01.zip")).toBe("My Gallery 01");
  expect(fileTitle("[Diogenes Club, Haikawa Hemlen] - NobaMei Correction.zip")).toBe("NobaMei Correction");
  expect(fileTitle("[Circle (Artist)] Title (Parody) [English] [Digital].zip")).toBe("Title (Parody)");
  expect(fileTitle("[Only Brackets].zip")).toBe("[Only Brackets]");
  expect(fileTitle("[Circle [Artist] Group] Title.zip")).toBe("Title");
  expect(fileTitle("【DL版】[Circle] タイトル 【English】.zip")).toBe("タイトル");
  expect(fileTitle("【Only】.zip")).toBe("【Only】");
  const s = server(rules);
  const { ref } = await createDraft(s.client, "gallery", "en", new File(["z"], "Resort_Lover.zip"), "Untitled");
  expect(s.posts).toEqual([{ kind: "gallery", text: { en: { title: "Resort Lover" } } }]);
  expect(ref).toEqual({ kind: "gallery", id, version: v1 });
});

it("routes a ZIP to the import path and an image to the pages, by the item's own rules", async () => {
  const s = server(rules);
  const ref = { kind: "gallery", id, version: v1 };
  // Some browsers type ZIPs application/x-zip-compressed; the rules' type wins.
  await expect(uploadTarget(s.client, ref, new File(["z"], "chapter.zip", { type: "application/x-zip-compressed" }))).resolves.toEqual({ path: "import/chapter.zip", type: "application/zip" });
  await expect(uploadTarget(s.client, ref, new File(["p"], "01.png", { type: "image/png" }))).resolves.toEqual({ path: "originals/01.png", type: "image/png" });
  await expect(uploadTarget(s.client, ref, new File(["t"], "notes.txt", { type: "text/plain" }))).rejects.toMatchObject({ code: "type_not_allowed" });
  // A video goes to its kind's literal source path.
  const video = server([{ path: "subs/{name}", types: ["text/vtt"], max_bytes: 0 }, { path: "source", types: ["video/mp4"], max_bytes: 0 }]);
  await expect(uploadTarget(video.client, ref, new File(["v"], "Episode_1.mp4", { type: "video/mp4" }))).resolves.toEqual({ path: "source", type: "video/mp4" });
  await expect(uploadTarget(server(undefined).client, ref, new File(["z"], "a.zip"))).rejects.toMatchObject({ code: "forbidden" });
});

it("applies chosen defaults to each draft using content, version and shared membership writes", async () => {
  const s = server(rules);
  const defaults = { format: "manga", language: "ja", access: { level: "tier" as const, tiers: ["premium"] }, artists: ["artist1", "artist2"] };
  for (const name of ["One.zip", "Two.zip"]) {
    const draft = await createDraft(s.client, "gallery", defaults.language, new File(["z"], name), "Untitled");
    await applyDraftDefaults(s.client, draft, model, defaults);
    expect(s.writes).toEqual(expect.arrayContaining([
      { method: "PATCH", path: `/contents/${draft.content.id}`, body: { attributes: { format: "manga" }, access: defaults.access } },
      { method: "PATCH", path: `/contents/${draft.content.id}/versions/${v1}`, body: { languages: ["ja"] } },
      { method: "PUT", path: `/contents/${draft.content.id}/entities/shared`, body: { kinds: ["artist"], memberships: [{ group_id: "artist1" }, { group_id: "artist2" }] } },
    ]));
  }
  expect(s.posts).toEqual(["One", "Two"].map((title) => ({ kind: "gallery", text: { ja: { title } } })));
  expect(s.writes).toHaveLength(8);
});

it("leaves empty defaults unset and makes no default writes", async () => {
  const s = server(rules);
  const draft = await createDraft(s.client, "gallery", "en", new File(["z"], "One.zip"), "Untitled");
  await expect(applyDraftDefaults(s.client, draft, model, { format: "", language: "", artists: [] })).resolves.toEqual(draft.content);
  expect(s.writes).toEqual([{ method: "POST", path: "/contents", body: { kind: "gallery", text: { en: { title: "One" } } } }]);
});

it("uses version writes for version-scoped access and artists", async () => {
  const s = server(rules);
  const draft = await createDraft(s.client, "gallery", "en", new File(["z"], "One.zip"), "Untitled");
  await applyDraftDefaults(s.client, draft, { ...model, kinds: { gallery: { ...model.kinds.gallery!, access_scope: "version" } }, entity_kinds: { artist: { route: "artists", membership_scopes: ["version"] } } }, { access: { level: "public" }, artists: ["artist1"] });
  expect(s.writes.slice(1)).toEqual([
    { method: "PATCH", path: `/contents/${id}/versions/${v1}`, body: { access: { level: "public" } } },
    { method: "PUT", path: `/contents/${id}/versions/${v1}/entities`, body: { kinds: ["artist"], memberships: [{ group_id: "artist1" }] } },
  ]);
});

it("rejects a draft's failed default without preventing other drafts from applying theirs", async () => {
  const s = server(rules, `/contents/${id}/entities/shared`);
  const drafts = await Promise.all(["One.zip", "Two.zip"].map((name) => createDraft(s.client, "gallery", "en", new File(["z"], name), "Untitled")));
  const results = await Promise.allSettled(drafts.map((draft) => applyDraftDefaults(s.client, draft, model, { artists: ["artist1"] })));
  expect(results[0]).toMatchObject({ status: "rejected", reason: { code: "invalid_request", message: "Choose another artist and retry." } });
  expect(results[1]).toMatchObject({ status: "fulfilled" });
});

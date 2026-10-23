import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, UploadRule } from "../../client/generated/wire.js";
import { createDraft, fileTitle, uploadTarget } from "./bulk-drafts.js";

const id = "01a14382-2b60-7019-a80a-3e24e24491c2";
const v1 = "01a14382-2b61-7019-a80a-3e24e24491c3";
const v2 = "01a14382-2b62-7019-a80a-3e24e24491c4";
const rules: UploadRule[] = [
  { path: "cover", types: ["image/png", "image/jpeg"], max_bytes: 0, max: 1 },
  { path: "covers/{name}", types: ["image/png", "image/jpeg"], max_bytes: 0 },
  { path: "originals/{name}", types: ["image/png", "image/jpeg"], max_bytes: 0, page: true },
  { path: "import/{name}", types: ["application/zip"], max_bytes: 0 },
];

function server(uploads: UploadRule[] | undefined) {
  const posts: unknown[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    if (init?.method === "POST" && url.pathname.endsWith("/contents")) {
      posts.push(JSON.parse(String(init.body)));
      const created = { id, kind: "gallery", versions: [{ id: v2, position: 2 }, { id: v1, position: 1 }] } as unknown as Content;
      return Response.json(created, { status: 201 });
    }
    if (url.pathname.includes("/media/gallery/")) return Response.json({ files: [], ...(uploads ? { uploads } : {}) });
    return Response.json({ code: "not_found", error: url.pathname }, { status: 404 });
  });
  return { client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }), posts };
}

it("titles a draft from its file name and uploads into its first version", async () => {
  expect(fileTitle("My_Gallery-01.zip")).toBe("My Gallery 01");
  expect(fileTitle("[Diogenes Club, Haikawa Hemlen] - NobaMei Correction.zip")).toBe("NobaMei Correction");
  expect(fileTitle("[Circle (Artist)] Title (Parody) [English] [Digital].zip")).toBe("Title (Parody)");
  expect(fileTitle("[Only Brackets].zip")).toBe("[Only Brackets]");
  expect(fileTitle("[Circle [Artist] Group] Title.zip")).toBe("Title");
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

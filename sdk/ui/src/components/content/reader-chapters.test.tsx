// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, FileInfo, ReadResult, Segment, SegmentInput, Version } from "../../client/generated/wire.js";
import { SegmentsEditor } from "./editors.js";

const hashes = { a: "a".repeat(64), b: "b".repeat(64), c: "c".repeat(64), d: "d".repeat(64) };
const model: ContentModel = { languages: ["en"], tiers: [], signals: false, kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" }, segments: true } }, entity_kinds: {} };
const source = (id: string, path = `pages/${id[0]}.png`): FileInfo => ({ path, page_id: id, type: "image/png", upload: true, url: `https://original.invalid/${path}` });
const chapter = (start: string, title = "Chapter"): Segment => ({ position: 1, start: 0, end: 0, start_page: start, title, released: true });
const version = (segments = [chapter(hashes.a, "First"), chapter(hashes.c, "Second")]): Version => ({ id: "v1", code: "EDITION1", position: 1, languages: ["en"], traits: [], state: "draft", live: false, has_audio: false, attributes: {}, segments, decision: { allowed: true } });
const content = (edition: Version): Content => ({
  id: "c1", code: "GALLERY1", kind: "gallery", path: "/g/GALLERY1", language: "en", title: "Gallery", decision: { allowed: true },
  state: "draft", listing: "listed", comments: "open", live: false, access: { level: "public" }, restrictions: [], attributes: {},
  creators: [], entities: [], versions: [edition], editor: true, revision: 1, created_at: "2026-10-18T00:00:00Z", updated_at: "2026-10-18T00:00:00Z",
});

function server(files: FileInfo[], edition = version()) {
  const writes: SegmentInput[][] = [], requests: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "https://test.invalid").pathname.replace(/^\/v1/, "");
    requests.push(path);
    if (path === "/kinds") return Response.json(model);
    if (init?.method === "PUT" && path === "/contents/c1/versions/v1/segments") {
      const segments = JSON.parse(String(init.body)).segments as SegmentInput[];
      writes.push(segments);
      return Response.json({ ...edition, segments: segments.map((segment) => ({ ...segment, start: 0, end: 0, released: true })) });
    }
    return Response.json({ code: "not_found", error: "Not found" }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  const read: ReadResult = { files, total: files.length, offset: 0, limit: 200, expires: 0, access: "full", uploads: [{ path: "pages/{name}", types: ["image/png"], max_bytes: 1000, page: true }] };
  const mediaRead = vi.spyOn(client.media, "read").mockResolvedValue(read);
  return { client, mediaRead, requests, writes };
}

it("authors unpublished reader chapters by source hash and derives their ranges", async () => {
  const s = server([source(hashes.a), source(hashes.b), source(hashes.c)]), edition = version(), user = userEvent.setup();
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  expect(await screen.findByText("Pages 1–2")).toBeVisible();
  expect(screen.getByText("Pages 3–3")).toBeVisible();
  expect(screen.queryByLabelText("Chapter 1 end")).toBeNull();
  expect(s.mediaRead).toHaveBeenCalledWith({ kind: "gallery", id: "c1", version: "v1" }, expect.objectContaining({ editor: true }));
  expect(s.requests.some((path) => path.includes("/reader"))).toBe(false);
  await user.click(screen.getByRole("button", { name: "Save chapters" }));
  await waitFor(() => expect(s.writes).toHaveLength(1));
  expect(s.writes[0]).toEqual([{ position: 1, title: "First", start_page: hashes.a }, { position: 2, title: "Second", start_page: hashes.c }]);
});

it("keeps a chapter on its source image when pages are inserted and renamed", async () => {
  const edition = version(), first = server([source(hashes.a), source(hashes.b), source(hashes.c)]), next = server([source(hashes.a, "pages/renamed.png"), source(hashes.d), source(hashes.b), source(hashes.c)]);
  const view = render(<SegmentsEditor content={content(edition)} version={edition} client={first.client} />);
  await screen.findByText("Pages 1–2");
  view.rerender(<SegmentsEditor content={content(edition)} version={edition} client={next.client} />);
  expect(await screen.findByText("Pages 1–3")).toBeVisible();
  expect(screen.getByText("Pages 4–4")).toBeVisible();
});

it("uses generated previews and never original image URLs in the chapter picker", async () => {
  const s = server([
    source(hashes.a), source(hashes.b), source(hashes.c),
    { path: "thumb/a.webp", type: "image/webp", from: "pages/a.png", w: 570, h: 806, url: "https://thumb.invalid/a.webp" },
    { path: "low/a.webp", type: "image/webp", from: "pages/a.png", w: 1206, h: 1706, url: "https://low.invalid/a.webp" },
  ]), edition = version(), user = userEvent.setup();
  const view = render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  await user.click(await screen.findByRole("button", { name: "Chapter 1 start" }));
  expect(view.baseElement.querySelector('img[src="https://thumb.invalid/a.webp"]')).not.toBeNull();
  expect(view.baseElement.querySelector('img[src^="https://original.invalid/"]')).toBeNull();
  expect(view.baseElement.querySelector('img[src^="https://low.invalid/"]')).toBeNull();
});

it("requires repair when a source hash disappears and disables ambiguous duplicate anchors", async () => {
  const edition = version([chapter(hashes.a)]), s = server([source(hashes.b), source(hashes.b, "pages/duplicate.png")]), user = userEvent.setup();
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  expect(await screen.findByText("This chapter’s start page is missing. Choose another page.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Save chapters" })).toBeDisabled();
  await user.click(screen.getByRole("button", { name: "Chapter 1 start" }));
  expect(screen.getByRole("button", { name: "Page 1: b.png" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Page 2: duplicate.png" })).toBeDisabled();
  expect(s.writes).toEqual([]);
});

it("does not save a chapter whose source hash occurs twice", async () => {
  const edition = version([chapter(hashes.a)]), s = server([source(hashes.a), source(hashes.a, "pages/duplicate.png")]);
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  expect(await screen.findByText("This image appears more than once. Choose a unique chapter-start page.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Save chapters" })).toBeDisabled();
});

it("converts legacy numeric chapter bounds to current source-hash starts when saved", async () => {
  const edition = version([{ position: 1, title: "Old chapter", start: 1, end: 3, released: true }]);
  const s = server([source(hashes.a), source(hashes.b), source(hashes.c)], edition), user = userEvent.setup();
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  expect(await screen.findByText("Pages 2–3")).toBeVisible();
  await user.click(screen.getByRole("button", { name: "Save chapters" }));
  await waitFor(() => expect(s.writes).toHaveLength(1));
  expect(s.writes[0]).toEqual([{ position: 1, title: "Old chapter", start_page: hashes.b }]);
});

it("rejects chapter starts that do not follow the current reading order", async () => {
  const edition = version(), s = server([source(hashes.a), source(hashes.b), source(hashes.c)]), user = userEvent.setup();
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  await user.click(await screen.findByRole("button", { name: "Chapter 2 start" }));
  await user.click(screen.getByRole("button", { name: "Page 1: a.png" }));
  expect(await screen.findByText("Chapter starts must follow page order.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Save chapters" })).toBeDisabled();
  expect(s.writes).toEqual([]);
});

it("does not treat an automatically excluded duplicate as an ambiguous start", async () => {
  const excluded = { ...source(hashes.a, "pages/duplicate.png"), meta: { page: { included: false } } };
  const edition = version([chapter(hashes.a)]), s = server([source(hashes.a), excluded, source(hashes.b)]);
  render(<SegmentsEditor content={content(edition)} version={edition} client={s.client} />);
  expect(await screen.findByText("Pages 1–2")).toBeVisible();
  expect(screen.queryByText("This image appears more than once. Choose a unique chapter-start page.")).toBeNull();
  expect(screen.getByRole("button", { name: "Save chapters" })).toBeEnabled();
});

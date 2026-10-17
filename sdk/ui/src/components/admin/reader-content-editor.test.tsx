// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, ContentMemberships, FileInfo, ReadResult, Version } from "../../client/generated/wire.js";
import { ContentEditor, ContentManager } from "./content-manager.js";
import { readerMediaReady } from "../content/editor-save.js";

const model: ContentModel = {
  languages: ["en", "ja"],
  tiers: [],
  signals: false,
  kinds: {
    gallery: { route: "g", presentation: "reader", versions: { languages: "one" }, entities: ["artist", "series", "character", "tag"], segments: true },
  },
  entity_kinds: { artist: { route: "artists" }, series: { route: "series" }, character: { route: "characters", scoped_by: "series" }, tag: { route: "tags" } },
};
const at = "2026-10-16T00:00:00Z";
const edition: Version = {
  id: "v1",
  code: "VERSION1",
  position: 1,
  languages: ["en"],
  traits: [],
  state: "published",
  live: false,
  has_audio: false,
  attributes: {},
  segments: [],
  decision: { allowed: true },
  pages: 1,
};
const gallery = (): Content => ({
  id: "c1",
  code: "GALLERY01",
  kind: "gallery",
  path: "/g/GALLERY01",
  language: "en",
  title: "Old title",
  decision: { allowed: true },
  state: "draft",
  listing: "listed",
  comments: "open",
  live: false,
  access: { level: "public" },
  restrictions: [],
  attributes: {},
  creators: [],
  entities: [],
  versions: [edition],
  editor: true,
  text: { en: { title: "Old title", description: "Old description" }, ja: { title: "旧タイトル" } },
  revision: 1,
  created_at: at,
  updated_at: at,
});
const page: FileInfo = { path: "pages/001.png", type: "image/png", upload: true, url: "https://test.invalid/page.png", size: 20, w: 600, h: 800 };
const read = (files: FileInfo[] = [page]): ReadResult => ({
  files,
  total: files.length,
  offset: 0,
  limit: 200,
  expires: 0,
  access: "full",
  uploads: [
    { path: "pages/{name}", types: ["image/png", "application/zip"], max_bytes: 10_000_000, page: true },
    { path: "cover/{name}", types: ["image/png"], max_bytes: 10_000_000, named: true },
  ],
});
const links: ContentMemberships = { content_id: "c1", memberships: [], entities: [] };

type Call = { method: string; path: string; body: unknown };
function server(options: { failText?: boolean; media?: ReadResult; memberships?: Promise<ContentMemberships>; noText?: boolean } = {}) {
  let item = gallery();
  if (options.noText) delete item.text;
  const calls: Call[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    const path = url.pathname.replace(/^\/v1/, "");
    const method = init?.method ?? "GET";
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push({ method, path, body });
    if (method === "GET" && path === "/kinds") return Response.json(model);
    if (method === "GET" && (path === "/manage/gallery" || path === "/manage")) {
      const code = url.searchParams.get("code");
      return Response.json(code && code !== item.code ? [] : [item]);
    }
    if (method === "GET" && path === "/contents/c1") return Response.json(item);
    if (method === "POST" && path === "/contents") {
      item = { ...item, text: body.text, title: Object.values(body.text as NonNullable<Content["text"]>)[0]!.title };
      return Response.json(item);
    }
    if (method === "GET" && path === "/contents/c1/entities") return Response.json(await (options.memberships ?? Promise.resolve(links)));
    if (method === "PUT" && path === "/contents/c1/entities") return Response.json({ ...links, memberships: body.memberships });
    if (method === "PUT" && path.startsWith("/contents/c1/text/")) {
      if (options.failText) return Response.json({ code: "conflict", error: "Changed elsewhere" }, { status: 409 });
      const lang = path.split("/").at(-1)!;
      item = { ...item, text: { ...item.text, [lang]: body }, title: body.title, revision: item.revision + 1 };
      return Response.json(item);
    }
    if (method === "PATCH" && path === "/contents/c1") {
      expect(body.revision).toBe(item.revision);
      item = { ...item, ...body, revision: item.revision + 1 };
      return Response.json(item);
    }
    if (method === "PATCH" && path === "/contents/c1/versions/v1") {
      item = { ...item, versions: [{ ...item.versions[0]!, ...body }] };
      return Response.json(item.versions[0]);
    }
    if (method === "GET" && path === "/contents/c1/versions/v1") return Response.json(item.versions[0]);
    if (method === "GET" && path === "/contents/c1/versions/v1/page-notices") return Response.json({ notices: [] });
    if (method === "POST" && path === "/contents/c1/publish") {
      item = { ...item, state: "published", live: true };
      return Response.json(item);
    }
    if (method === "GET" && path === "/typeahead") return Response.json({ contents: [], entities: [] });
    return Response.json({ code: "not_found", error: `Unexpected ${method} ${path}` }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  const media = vi.spyOn(client.media, "read").mockImplementation(async () => options.media ?? read());
  return {
    client,
    calls,
    media,
    get item() {
      return item;
    },
  };
}
const writes = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method !== "GET");

it("opens a complete reader workspace without a create-draft step or writes on mount", async () => {
  const s = server();
  render(<ContentEditor client={s.client} kind="gallery" id={null} />);
  expect(await screen.findByLabelText("Title")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Create draft" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save draft" })).toBeVisible();
  expect(screen.getByLabelText("Gallery files")).toBeInTheDocument();
  expect(screen.getAllByRole("heading", { name: "Gallery files" })).toHaveLength(1);
  expect(screen.getByRole("combobox", { name: "Add: Artist" })).toBeVisible();
  expect(screen.getByRole("combobox", { name: "Add: Series" })).toBeVisible();
  expect(screen.getByRole("combobox", { name: "Add: Character" })).toBeVisible();
  expect(screen.getByRole("combobox", { name: "Add: Tag" })).toBeVisible();
  expect(writes(s)).toEqual([]);
  expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
});

it("loads an editor response that supplies its localized fields without a text map", async () => {
  const s = server({ noText: true });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  expect(await screen.findByLabelText("Title")).toHaveValue("Old title");
});

it("allocates the technical draft only when saving and keeps the edited title", async () => {
  const s = server(),
    user = userEvent.setup(),
    onCreated = vi.fn();
  render(<ContentEditor client={s.client} kind="gallery" id={null} onCreated={onCreated} />);
  await user.type(await screen.findByLabelText("Title"), "New gallery");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("Saved.", { selector: "p[role=status]" })).toBeVisible();
  expect(writes(s).filter((c) => c.path === "/contents")).toHaveLength(1);
  expect(onCreated).toHaveBeenCalledOnce();
  expect(screen.getByLabelText("Title")).toHaveValue("New gallery");
  expect(s.item.text?.en?.title).toBe("New gallery");
  expect(writes(s).some((c) => c.path.endsWith("/publish"))).toBe(false);
});

it("saves the visible text and current revision before publishing", async () => {
  const s = server(),
    user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  const title = await screen.findByLabelText("Title");
  await user.clear(title);
  await user.type(title, "New title");
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents/c1/publish")).toBe(true));
  expect(writes(s).map((c) => `${c.method} ${c.path}`)).toEqual(["PUT /contents/c1/text/en", "PATCH /contents/c1", "POST /contents/c1/publish"]);
  expect(s.item.title).toBe("New title");
});

it("keeps unsaved input and refuses publication when saving fails", async () => {
  const s = server({ failText: true }),
    user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  const title = await screen.findByLabelText("Title");
  await user.clear(title);
  await user.type(title, "Keep me");
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save draft" })).toBeEnabled());
  expect(title).toHaveValue("Keep me");
  expect(writes(s).some((c) => c.path.endsWith("/publish"))).toBe(false);
});

it("retains language drafts when switching languages and saves both", async () => {
  const s = server(),
    user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  const title = await screen.findByLabelText("Title");
  await user.clear(title);
  await user.type(title, "English edit");
  await user.click(screen.getByRole("combobox", { name: "Language" }));
  await user.click(await screen.findByRole("option", { name: "ja" }));
  await waitFor(() => expect(title).toHaveValue("旧タイトル"));
  await user.clear(title);
  await user.type(title, "日本語の編集");
  await user.click(screen.getByRole("combobox", { name: "Language" }));
  await user.click(await screen.findByRole("option", { name: "en" }));
  expect(title).toHaveValue("English edit");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s).filter((c) => c.method === "PUT")).toHaveLength(2));
  expect(s.item.text?.en?.title).toBe("English edit");
  expect(s.item.text?.ja?.title).toBe("日本語の編集");
});

it("does not expose entity edits until existing associations finish loading", async () => {
  let resolve!: (links: ContentMemberships) => void;
  const memberships = new Promise<ContentMemberships>((done) => {
    resolve = done;
  });
  const s = server({ memberships });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  expect(await screen.findByRole("combobox", { name: "Add: Artist" })).toBeDisabled();
  resolve(links);
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Add: Artist" })).toBeEnabled());
});

it("prevents publishing a processing or failed page", async () => {
  const s = server({ media: read([{ ...page, failed: { of: "image", message: "Decoder failed" } }]) });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  expect(await screen.findByText(/Some files need attention/)).toBeVisible();
  expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
  expect(writes(s)).toEqual([]);
});

it("keeps the editor mounted when its internal draft changes the route", async () => {
  const s = server(),
    user = userEvent.setup();
  function Route() {
    const [path, setPath] = useState("/galleries/new");
    return <ContentManager client={s.client} kind="gallery" basePath="/galleries" navigation={{ pathname: path, search: "", navigate: (to) => setPath(to) }} />;
  }
  render(<Route />);
  const title = await screen.findByLabelText("Title");
  await user.type(title, "Stable editor");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s).some((c) => c.method === "PATCH")).toBe(true));
  expect(screen.getByLabelText("Title")).toBe(title);
  expect(title).toHaveValue("Stable editor");
});

it("checks every media window before publication, including late failures", async () => {
  const s = server();
  s.media.mockImplementation(async (_, options) => ({
    ...read(options?.offset ? [{ ...page, failed: { of: "image", message: "Late failure" } }] : [page]),
    offset: options?.offset ?? 0,
    total: 250,
  }));
  await expect(readerMediaReady(s.client, { kind: "gallery", id: "c1", version: "v1" })).resolves.toBe(false);
  expect(s.media.mock.calls.map(([, o]) => o?.offset)).toEqual([0, 200]);
});

it("saves an advanced version edit before publishing even after its panel closes", async () => {
  const s = server(),
    user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  await user.click(screen.getByText("Versions and chapters", { selector: "summary" }));
  const label = await screen.findByLabelText("Label (optional)");
  await user.type(label, "Revised edition");
  await user.click(screen.getByText("Versions and chapters", { selector: "summary" }));
  await user.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents/c1/publish")).toBe(true));
  const order = writes(s).map((c) => c.path);
  expect(order.indexOf("/contents/c1/versions/v1")).toBeLessThan(order.indexOf("/contents/c1/publish"));
  expect(s.item.versions[0]?.label).toBe("Revised edition");
});

it("sets the thumbnail from a selected page without fetching or reuploading its bytes", async () => {
  const s = server(),
    user = userEvent.setup();
  vi.spyOn(s.client.media, "presetPath").mockResolvedValue("cover/i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.png");
  const commit = vi.spyOn(s.client.media, "commit").mockResolvedValue([]);
  const set = vi.spyOn(s.client.contents, "setCover").mockResolvedValue(edition);
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await user.click(await screen.findByRole("button", { name: "Reorder 01.png" }));
  await user.click(await screen.findByRole("button", { name: "Set as thumbnail" }));
  await waitFor(() => expect(set).toHaveBeenCalledOnce());
  expect(commit).toHaveBeenCalledWith({ kind: "gallery", id: "c1", version: "v1" }, [
    { op: "copy", from: { id: "c1", version: "v1", path: "pages/001.png" }, to: "cover/i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.png" },
  ]);
  expect(set).toHaveBeenCalledWith("c1", "v1", "i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b");
});

it("starts a fresh editor when the app client or content target changes", async () => {
  const a = server(),
    b = server(),
    user = userEvent.setup();
  const { rerender } = render(<ContentEditor client={a.client} kind="gallery" id="c1" />);
  const original = await screen.findByLabelText("Title");
  await user.clear(original);
  await user.type(original, "Private unsaved edit");
  rerender(<ContentEditor client={b.client} kind="gallery" id="c1" />);
  await waitFor(() => expect(screen.getByLabelText("Title")).toHaveValue("Old title"));
  expect(screen.getByLabelText("Title")).not.toBe(original);
  rerender(<ContentEditor client={b.client} kind="gallery" id={null} />);
  expect(screen.getByLabelText("Title")).toHaveValue("");
  expect(writes(a)).toEqual([]);
  expect(writes(b)).toEqual([]);
});

it("allocates one hidden draft on file selection and carries pending uploads through its URL change", async () => {
  const s = server();
  const upload = vi.spyOn(s.client.media, "upload").mockImplementation(() => new Promise(() => {}));
  function Route() {
    const [path, setPath] = useState("/galleries/new");
    return <ContentManager client={s.client} kind="gallery" basePath="/galleries" navigation={{ pathname: path, search: "", navigate: (to) => setPath(to) }} />;
  }
  render(<Route />);
  const input = await screen.findByLabelText("Gallery files");
  fireEvent.change(input, { target: { files: [new File(["page one"], "first.png", { type: "image/png" })] } });
  // Adding the first file replaces the empty drop-zone input with the toolbar input.
  fireEvent.change(screen.getByLabelText("Gallery files"), { target: { files: [new File(["page two"], "second.png", { type: "image/png" })] } });
  await waitFor(() => expect(upload).toHaveBeenCalledTimes(2));
  expect(writes(s).filter((c) => c.path === "/contents")).toHaveLength(1);
  expect(screen.getByLabelText("Title")).toHaveValue("first");
  expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
  expect(upload.mock.calls.every(([, o]) => o.ref.id === "c1" && o.ref.version === "v1")).toBe(true);
});

it("keeps the draft editing session when the built-in history router changes synchronously", async () => {
  const s = server();
  const user = userEvent.setup();
  window.history.replaceState(null, "", "/standalone/galleries/new");
  try {
    render(<ContentManager client={s.client} kind="gallery" basePath="/standalone/galleries" />);
    const title = await screen.findByLabelText("Title");
    await user.type(title, "Synchronous route");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(window.location.pathname).toBe("/standalone/galleries/GALLERY01"));
    await screen.findByText("Saved.", { selector: "p[role=status]" });
    expect(screen.getByLabelText("Title")).toBe(title);
    expect(title).toHaveValue("Synchronous route");
  } finally {
    window.history.replaceState(null, "", "/");
  }
});

it("opens an editor URL by content code and reports an unknown code", async () => {
  const s = server();
  const manage = vi.spyOn(s.client.contents, "manage");
  function Route({ path }: { path: string }) {
    return <ContentManager client={s.client} kind="gallery" basePath="/galleries" navigation={{ pathname: path, search: "", navigate: () => {} }} />;
  }
  const { rerender } = render(<Route path="/galleries/GALLERY01" />);
  expect(await screen.findByLabelText("Title")).toHaveValue("Old title");
  expect(manage).toHaveBeenCalledWith({ kind: "gallery", code: "GALLERY01", limit: 1 }, expect.any(AbortSignal));
  rerender(<Route path="/galleries/MISSING01" />);
  expect(await screen.findByText("This item doesn't exist or isn't yours to manage.")).toBeVisible();
});

it("refreshes pages and notices after asynchronous ZIP extraction replaces the archive with pages", async () => {
  const options = {
    media: read([{ path: "pages/gallery.zip", type: "application/zip", upload: true, size: 100, pending: ["extract"] }]),
  };
  const s = server(options);
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  await waitFor(() => expect(s.calls.some((call) => call.path.endsWith("/page-notices"))).toBe(true));
  expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
  options.media = read([page]);
  // Exercise the normal editor-read polling, without a new client-side commit
  // or media.processed event to refresh the notices separately.
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(), { timeout: 6_000 });
  expect(s.calls.filter((call) => call.path.endsWith("/page-notices")).length).toBeGreaterThan(1);
});

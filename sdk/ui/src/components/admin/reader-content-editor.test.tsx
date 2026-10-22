// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, ContentMemberships, FileInfo, GroupCard, ReadResult, Version } from "../../client/generated/wire.js";
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
  entities: [],
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
function server(options: { failText?: boolean; failPublish?: boolean; media?: ReadResult; memberships?: Promise<ContentMemberships>; noText?: boolean; model?: ContentModel; versions?: Version[] } = {}) {
  let item = gallery();
  if (options.versions) item.versions = options.versions;
  let memberState: ContentMemberships | undefined;
  const members = async () => memberState ??= await (options.memberships ?? Promise.resolve(links));
  if (options.noText) delete item.text;
  const calls: Call[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    const path = url.pathname.replace(/^\/v1/, "");
    const method = init?.method ?? "GET";
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push({ method, path, body });
    if (method === "GET" && path === "/kinds") return Response.json(options.model ?? model);
    if (method === "GET" && (path === "/manage/gallery" || path === "/manage")) {
      const code = url.searchParams.get("code");
      const deleted = url.searchParams.get("state") === "deleted";
      return Response.json(code ? code === item.code ? [item] : [] : (item.state === "deleted") === deleted ? [item] : []);
    }
    if (method === "GET" && path === "/manage/gallery/count") return Response.json({ total: (item.state === "deleted") === (url.searchParams.get("state") === "deleted") ? 1 : 0 });
    if (method === "GET" && path === "/contents/c1") return Response.json(item);
    if (method === "POST" && path === "/contents") {
      item = { ...item, ...body, text: body.text, title: Object.values(body.text as NonNullable<Content["text"]>)[0]!.title };
      return Response.json(item);
    }
    if (method === "GET" && path === "/contents/c1/entities") return Response.json(await members());
    if (method === "PUT" && (path === "/contents/c1/entities/shared" || /^\/contents\/c1\/versions\/[^/]+\/entities$/.test(path))) {
      const before = await members();
      const version = path === "/contents/c1/entities/shared" ? undefined : path.split("/")[4];
      const kept = before.memberships.filter((m) => m.version_id !== version || !body.kinds.includes(before.entities.find((g) => g.id === m.group_id)?.kind));
      memberState = { ...before, memberships: [...kept, ...body.memberships.map((m: object) => ({ ...m, ...(version ? { version_id: version } : {}) }))] };
      return Response.json(memberState);
    }
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
    const editedVersion = /^\/contents\/c1\/versions\/([^/]+)$/.exec(path)?.[1];
    if (method === "PATCH" && editedVersion) {
      item = { ...item, versions: item.versions.map((v) => v.id === editedVersion ? { ...v, ...body } : v) };
      if (item.state === "deleted" && body.state === "draft") item = { ...item, state: "published", deleted_at: undefined };
      return Response.json(item.versions.find((v) => v.id === editedVersion));
    }
    if (method === "DELETE" && editedVersion) {
      item = { ...item, versions: item.versions.map((v) => v.id === editedVersion ? { ...v, state: "deleted" } : v) };
      if (item.versions.every((v) => v.state === "deleted")) item = { ...item, state: "deleted", live: false, deleted_at: at };
      return new Response(null, { status: 204 });
    }
    if (method === "GET" && editedVersion) return Response.json(item.versions.find((v) => v.id === editedVersion));
    if (method === "GET" && path.endsWith("/page-notices")) return Response.json({ notices: [] });
    if (method === "POST" && path === "/contents/c1/publish") {
      if (options.failPublish) return Response.json({ code: "conflict", error: "Publication changed elsewhere" }, { status: 409 });
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
  expect(screen.getByRole("button", { name: "Save draft" })).toBeDisabled(); // nothing to save yet
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
  // An unedited existing gallery says so instead of offering a save.
  expect(screen.getByText("All changes saved")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Save draft" })).not.toBeInTheDocument();
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
  expect(screen.getByText("All changes saved")).toBeVisible();
  await user.type(screen.getByLabelText("Title"), "!");
  expect(screen.getByRole("button", { name: "Save draft" })).toBeEnabled();
  expect(screen.queryByText("All changes saved")).not.toBeInTheDocument();
});

it("requires configured gallery format and preserves the host's CGset label", async () => {
  const requiredModel: ContentModel = { ...model, kinds: { ...model.kinds, gallery: { ...model.kinds.gallery!, attributes: { format: { type: "enum", values: ["doujin", "cgset"], required: true } } } } };
  const s = server({ model: requiredModel }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" labels={{ cgset: "CGset" }} />);
  await user.type(await screen.findByLabelText("Title"), " edited");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("Format is required")).toBeVisible();
  expect(writes(s)).toEqual([]);
  await user.click(screen.getByRole("combobox", { name: "Format" }));
  expect(screen.queryByRole("option", { name: "None" })).not.toBeInTheDocument();
  await user.click(await screen.findByRole("option", { name: "CGset" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("Saved.", { selector: "p[role=status]" })).toBeVisible();
  expect(s.item.attributes.format).toBe("cgset");
});

it("starts a new gallery linked to the entity it was created from", async () => {
  const s = server(),
    user = userEvent.setup();
  const series = { id: "s1", kind: "series", slug: "saga", name: "Saga", internal: false, count: 0, attributes: {}, created_at: at } as GroupCard;
  render(<ContentEditor client={s.client} kind="gallery" id={null} entities={[series]} />);
  expect(await screen.findByText("Saga")).toBeVisible();
  await user.type(await screen.findByLabelText("Title"), "First chapter");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("Saved.", { selector: "p[role=status]" })).toBeVisible();
  const linked = writes(s).find((c) => c.method === "PUT" && c.path === "/contents/c1/entities/shared")?.body as { memberships?: unknown } | undefined;
  expect(linked?.memberships).toEqual([{ group_id: "s1" }]);
});

it("starts from a shared tag without duplicating it onto the first version", async () => {
  const sharedTagModel: ContentModel = { ...model, entity_kinds: { ...model.entity_kinds, tag: { membership_scopes: ["content", "version"] } } };
  const s = server({ model: sharedTagModel }), user = userEvent.setup();
  const tag: GroupCard = { id: "t1", kind: "tag", slug: "parent-tag", name: "Parent tag", internal: false, count: 0, attributes: {}, created_at: at };
  render(<ContentEditor client={s.client} kind="gallery" id={null} entities={[tag]} />);
  expect(await screen.findByText("Parent tag")).toBeVisible();
  await user.type(screen.getByLabelText("Title"), "Tagged gallery");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await screen.findByText("Saved.", { selector: "p[role=status]" });
  expect(writes(s).some((call) => call.path === "/contents/c1/entities/shared")).toBe(true);
  expect(writes(s).some((call) => call.path === "/contents/c1/versions/v1/entities")).toBe(false);
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
  expect(writes(s).map((c) => `${c.method} ${c.path}`)).toEqual(["PUT /contents/c1/text/en", "POST /contents/c1/publish"]);
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

it("saves metadata before committing removals and retains undo if metadata fails", async () => {
  const second = { ...page, path: "pages/second.png" };
  const s = server({ failText: true, media: read([page, second]) }), user = userEvent.setup();
  const commit = vi.spyOn(s.client.media, "commit");
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByRole("button", { name: "Reorder 001.png" });
  await user.click(screen.getAllByRole("button", { name: "Remove" })[0]!);
  await user.type(screen.getByLabelText("Title"), " modified");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save draft" })).toBeEnabled());
  expect(commit).not.toHaveBeenCalled();
  await user.click(screen.getByRole("button", { name: "Undo" }));
  await user.click(screen.getByRole("button", { name: "Undo" }));
  expect(screen.getByRole("button", { name: "Reorder 001.png" })).toBeVisible();
});

it("reports a separate publication failure after confirming saved metadata", async () => {
  const s = server({ failPublish: true }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await user.type(await screen.findByLabelText("Title"), " modified");
  await user.click(screen.getByRole("button", { name: "Publish" }));
  expect(await screen.findByText(/Changes saved\. Publishing failed:/)).toBeVisible();
  expect(s.item.title).toBe("Old title modified");
  expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();
});

it("guards a ready manual upload even when no metadata is dirty", async () => {
  const s = server({ versions: [{ ...edition, live: true }], media: read([page, { ...page, path: "pages/pending.png", unattached: true }]) });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByRole("button", { name: "Reorder 001.png" });
  const event = new Event("beforeunload", { cancelable: true });
  await waitFor(() => { window.dispatchEvent(event); expect(event.defaultPrevented).toBe(true); });
});

it("leaves text-field undo to the focused editor", async () => {
  const s = server(), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await user.type(await screen.findByLabelText("Title"), " modified");
  const search = screen.getByRole("combobox", { name: "Add: Artist" });
  await user.type(search, "search");
  const event = new KeyboardEvent("keydown", { key: "z", ctrlKey: true, bubbles: true, cancelable: true });
  search.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
  expect(screen.getByLabelText("Title")).toHaveValue("Old title modified");
});

it("keeps existing filenames on load and allows an individual staged rename", async () => {
  const s = server({ media: read([{ ...page, path: "pages/02_2.png" }, { ...page, path: "pages/03_3.png" }]) }), user = userEvent.setup();
  const commit = vi.spyOn(s.client.media, "commit");
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByRole("button", { name: "Reorder 02_2.png" });
  expect(screen.queryByRole("button", { name: "Clean filenames" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "02_2.png" }));
  const rename = screen.getByRole("textbox", { name: "Rename" });
  await user.clear(rename);
  await user.type(rename, "02.png{Enter}");
  expect(await screen.findByRole("button", { name: "Reorder 02.png" })).toBeVisible();
  expect(screen.getByRole("button", { name: "Reorder 03_3.png" })).toBeVisible();
  expect(commit).not.toHaveBeenCalled();
  await user.click(screen.getByRole("button", { name: "Undo" }));
  expect(screen.getByRole("button", { name: "Reorder 02_2.png" })).toBeVisible();
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
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents")).toBe(true));
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

it("saves the selected version edit before publishing without a combined versions panel", async () => {
  const s = server(),
    user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  expect(screen.queryByText("Versions and chapters")).toBeNull();
  const release = await screen.findByLabelText("Goes live");
  fireEvent.change(release, { target: { value: "2027-01-01T12:00" } });
  expect(screen.queryByText("Versions and chapters")).toBeNull();
  await user.click(screen.getByRole("button", { name: "Schedule" }));
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents/c1/publish")).toBe(true));
  const order = writes(s).map((c) => c.path);
  expect(order.indexOf("/contents/c1/versions/v1")).toBeLessThan(order.indexOf("/contents/c1/publish"));
  expect(s.item.versions[0]?.publish_at).toBeTruthy();
});

it("sets the thumbnail from a selected page without fetching or reuploading its bytes", async () => {
  const s = server(),
    user = userEvent.setup();
  vi.spyOn(s.client.media, "presetPath").mockResolvedValue("cover/i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.png");
  const commit = vi.spyOn(s.client.media, "commit").mockResolvedValue([]);
  const set = vi.spyOn(s.client.contents, "setCover").mockResolvedValue(edition);
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await user.click(await screen.findByRole("button", { name: "Reorder 001.png" }));
  await user.click(screen.getByRole("button", { name: "Thumbnail" }));
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
    await waitFor(() => expect(window.location.pathname).toBe("/standalone/galleries/GALLERY01/versions/VERSION1"));
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


it("the reader workspace follows the host's subscription-only model", async () => {
  const limited: ContentModel = { ...model, tiers: ["premium"], kinds: { gallery: { ...model.kinds.gallery!, access_levels: ["public", "tier"], comment_modes: ["open"] } } };
  const s = server({ model: limited });
  const user = userEvent.setup();
  const { container } = render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  expect(await screen.findByRole("heading", { name: "Access" })).toBeVisible();
  expect(screen.queryByRole("heading", { name: "Access and price" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Comments" })).toBeNull();
  expect(container.querySelector('[data-ckui="offer-fields"]')).toBeNull();
  await user.click(screen.getByRole("combobox", { name: "Who can open it" }));
  expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Everyone", "Tier members"]);
  await user.click(await screen.findByRole("option", { name: "Tier members" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(s.calls.some((c) => c.method === "PATCH" && (c.body as Partial<Content> | undefined)?.access?.tiers?.[0] === "premium")).toBe(true));
});

it("edits only the selected version's access in a version-scoped model", async () => {
  const direct: ContentModel = { ...model, tiers: ["premium"], kinds: { gallery: { ...model.kinds.gallery!, access_scope: "version", access_levels: ["public", "tier"], comment_modes: ["open"] } } };
  const s = server({ model: direct, versions: [edition, { ...edition, id: "v2", code: "VERSION2", label: "Keep this label" }] }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  const access = await screen.findByRole("combobox", { name: "Who can open it" });
  expect(screen.getAllByRole("combobox", { name: "Who can open it" })).toHaveLength(1);
  expect(screen.queryByText("Its own access rule")).toBeNull();
  expect(screen.queryByLabelText("Label (optional)")).toBeNull();
  expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  expect(screen.getByRole("button", { name: "Delete version" })).toBeVisible();
  await user.click(access);
  await user.click(await screen.findByRole("option", { name: "Tier members" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s).some((call) => call.path === "/contents/c1/versions/v2")).toBe(true));
  expect(writes(s).every((call) => call.path === "/contents/c1/versions/v2")).toBe(true);
  expect(s.item.versions[1]?.access).toEqual({ level: "tier", tiers: ["premium"] });
  expect(s.item.versions[1]?.label).toBe("Keep this label");
  expect(s.item.versions[0]?.access).toBeUndefined();
  expect(s.item.access).toEqual({ level: "public" });
});

it("publishes an internal draft version only after the explicit header action", async () => {
  const s = server({ versions: [{ ...edition, state: "draft" }] }), user = userEvent.setup();
  s.item.state = "published";
  s.item.live = true;
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  const release = await screen.findByLabelText("Goes live");
  expect(release.closest('[data-ckui="version-schedule-header"]')).not.toBeNull();
  expect(screen.queryByLabelText("Label (optional)")).toBeNull();
  expect(writes(s)).toEqual([]);
  await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(() => expect(s.item.versions[0]?.state).toBe("published"));
  expect(writes(s).map((call) => call.path)).toEqual(["/contents/c1/versions/v1"]);
});


it("keeps publication scheduling separate from access without listing controls", async () => {
  const limited: ContentModel = { ...model, tiers: ["premium"], kinds: { gallery: { ...model.kinds.gallery!, access_levels: ["public", "tier"], comment_modes: ["open"] } } };
  const s = server({ model: limited });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  expect(await screen.findByLabelText("Goes live")).toBeVisible();
  expect(screen.queryByRole("combobox", { name: "Listing" })).toBeNull();
  expect(screen.getByRole("combobox", { name: "Who can open it" })).toBeVisible();
});


it("a new reader item uses restricted defaults before its first create request", async () => {
  const limited: ContentModel = { ...model, tiers: ["premium"], kinds: { gallery: { ...model.kinds.gallery!, access_levels: ["tier"], comment_modes: ["closed"] } } };
  const s = server({ model: limited });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id={null} />);
  await user.type(await screen.findByLabelText("Title"), "Members only");
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(s.calls.find((c) => c.method === "POST" && c.path === "/contents")?.body).toMatchObject({ access: { level: "tier", tiers: ["premium"] }, comments: "closed" }));
});


const scopedModel: ContentModel = {
  ...model,
  kinds: { gallery: { ...model.kinds.gallery!, entities: ["artist", "tag", "voice_actor", "translator"], versions: { languages: "one", traits: ["colored"] } } },
  entity_kinds: { ...model.entity_kinds, voice_actor: { version_level: true }, translator: { version_level: true } },
};
const otherEdition: Version = { ...edition, id: "v2", code: "VERSION2", languages: ["en"], traits: [] };
const scopedLinks: ContentMemberships = {
  content_id: "c1",
  memberships: [{ group_id: "artist" }, { group_id: "actor1", version_id: "v1" }, { group_id: "actor2", version_id: "v2" }],
  entities: [
    { id: "artist", code: "ARTIST", kind: "artist", slug: "shared-artist", name: "Shared artist", internal: false, count: 1, attributes: {}, created_at: at },
    { id: "actor1", code: "ACTOR1", kind: "voice_actor", slug: "actor-one", name: "Actor one", internal: false, count: 1, attributes: {}, created_at: at },
    { id: "actor2", code: "ACTOR2", kind: "voice_actor", slug: "actor-two", name: "Actor two", internal: false, count: 1, attributes: {}, created_at: at },
  ],
};

it("a version URL loads only that version's files and credits", async () => {
  const s = server({ model: scopedModel, versions: [edition, otherEdition], memberships: Promise.resolve(scopedLinks) });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  expect(await screen.findByText("Actor two")).toBeVisible();
  expect(screen.queryByText("Actor one")).toBeNull();
  expect(screen.getByText("Shared artist")).toBeVisible();
  await waitFor(() => expect(s.media).toHaveBeenCalled());
  expect(s.media.mock.calls.every(([ref]) => ref.version === "v2")).toBe(true);
  expect(screen.queryByRole("combobox", { name: /Version for Actor/ })).toBeNull();
});

it("uses the shared Save and ordinary version tags instead of trait or state controls", async () => {
  const direct: ContentModel = { ...scopedModel, kinds: { gallery: { ...scopedModel.kinds.gallery!, access_scope: "version" } } };
  const s = server({ model: direct, versions: [edition, otherEdition] });
  const { container } = render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  expect(await screen.findByLabelText("Goes live")).toBeVisible();
  expect(screen.queryByRole("switch", { name: /colored|uncensored|decensored|manhwa/i })).toBeNull();
  expect(screen.queryByText("Its own access rule")).toBeNull();
  expect(screen.queryByLabelText("Label (optional)")).toBeNull();
  expect(screen.queryByRole("combobox", { name: "State" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect(screen.getByRole("button", { name: "Delete version" }).closest('[data-ckui="version-destructive-actions"]')).not.toBeNull();
  expect(container.querySelectorAll('[data-ckui="editor-thumbnail"]')).toHaveLength(1);
  expect(writes(s)).toEqual([]);
});

it("unpublishes only the selected edition", async () => {
  const s = server({ versions: [edition, otherEdition] }), user = userEvent.setup();
  s.item.state = "published";
  s.item.live = true;
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  await user.click(await screen.findByRole("button", { name: "Unpublish" }));
  await waitFor(() => expect(s.item.versions[1]?.state).toBe("draft"));
  expect(s.item.versions[0]?.state).toBe("published");
  expect(s.item.state).toBe("published");
  expect(writes(s).map((call) => [call.path, call.body])).toEqual([["/contents/c1/versions/v2", { state: "draft" }]]);
});

it("confirms version deletion and preserves unsaved changes when cancelled", async () => {
  const s = server({ versions: [edition, otherEdition] }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  const title = await screen.findByLabelText("Title");
  await user.type(title, " edited");
  await user.click(screen.getByRole("button", { name: "Delete version" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Delete version" });
  expect(within(dialog).getByText(/Only this version will be deleted/)).toBeVisible();
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(title).toHaveValue("Old title edited");
  expect(writes(s)).toEqual([]);
});

it("editing version credits targets only its atomic version scope", async () => {
  const s = server({ model: scopedModel, versions: [edition, otherEdition], memberships: Promise.resolve(scopedLinks) });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  await user.click(await screen.findByRole("button", { name: "Remove Actor two" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents/c1/versions/v2/entities")).toBe(true));
  expect(writes(s).map((c) => c.path)).toEqual(["/contents/c1/versions/v2/entities"]);
});

it("edits arbitrary version tags separately from shared gallery tags", async () => {
  const tag = (id: string, name: string): GroupCard => ({ id, code: id.toUpperCase(), kind: "tag", slug: id, name, internal: false, count: 1, attributes: {}, created_at: at });
  const linked: ContentMemberships = {
    ...scopedLinks,
    memberships: [...scopedLinks.memberships, { group_id: "shared-tag" }, { group_id: "colored", version_id: "v1" }, { group_id: "uncensored", version_id: "v2" }],
    entities: [...scopedLinks.entities, tag("shared-tag", "Shared tag"), tag("colored", "Colored"), tag("uncensored", "Uncensored")],
  };
  const scopedTags: ContentModel = { ...scopedModel, kinds: { ...scopedModel.kinds, gallery: { ...scopedModel.kinds.gallery!, versions: { languages: "one", traits: [] } } }, entity_kinds: { ...scopedModel.entity_kinds, tag: { membership_scopes: ["content", "version"] } } };
  const s = server({ model: scopedTags, versions: [edition, otherEdition], memberships: Promise.resolve(linked) }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  expect(await screen.findByText("Uncensored")).toBeVisible();
  expect(screen.getByText("Shared tag")).toBeVisible();
  expect(screen.queryByText("Colored")).toBeNull();
  expect(screen.getAllByRole("combobox", { name: "Add: Tag" })).toHaveLength(2);
  await user.click(screen.getByRole("button", { name: "Remove Uncensored" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s)).toHaveLength(1));
  expect(writes(s)[0]!.path).toBe("/contents/c1/versions/v2/entities");
  expect(writes(s)[0]!.body).toMatchObject({ kinds: expect.arrayContaining(["tag"]), memberships: [{ group_id: "actor2", version_id: "v2" }] });
});

it("saves dirty version details alongside its scoped credits", async () => {
  const s = server({ model: scopedModel, versions: [edition, otherEdition], memberships: Promise.resolve(scopedLinks) });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  fireEvent.change(await screen.findByLabelText("Goes live"), { target: { value: "2027-01-01T12:00" } });
  await user.click(await screen.findByRole("button", { name: "Remove Actor two" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await screen.findByText("Saved.", { selector: "p[role=status]" });
  expect(s.item.versions[1]?.publish_at).toBeTruthy();
  expect(writes(s).map((call) => call.path)).toContain("/contents/c1/versions/v2/entities");
  expect(writes(s).map((call) => call.path)).toContain("/contents/c1/versions/v2");
});

it("shared gallery entities use the parent-only atomic scope from any version", async () => {
  const s = server({ model: scopedModel, versions: [edition, otherEdition], memberships: Promise.resolve(scopedLinks) });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  await user.click(await screen.findByRole("button", { name: "Remove Shared artist" }));
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await waitFor(() => expect(writes(s).some((c) => c.path === "/contents/c1/entities/shared")).toBe(true));
  expect(writes(s).map((c) => c.path)).toEqual(["/contents/c1/entities/shared"]);
});

it("rejects an unknown version URL without opening a sibling's media", async () => {
  const s = server({ versions: [edition, otherEdition] });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="MISSING" />);
  expect(await screen.findByText("This version could not be found.")).toBeVisible();
  expect(s.media).not.toHaveBeenCalled();
});


it("navigates between version pages and confirms before discarding shared edits", async () => {
  const versions = [{ ...edition, label: "Edition one" }, { ...otherEdition, label: "Edition two" }];
  const s = server({ model: scopedModel, versions, memberships: Promise.resolve(scopedLinks) });
  const user = userEvent.setup();
  function Route() {
    const [path, setPath] = useState("/galleries/GALLERY01/versions/VERSION1");
    return <><output aria-label="Editor URL">{path}</output><ContentManager client={s.client} kind="gallery" basePath="/galleries" navigation={{ pathname: path, search: "", navigate: (to) => setPath(to) }} /></>;
  }
  render(<Route />);
  const title = await screen.findByLabelText("Title");
  await screen.findByText("Actor one");
  await user.type(title, " unsaved");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Versions" })).toBeEnabled());
  await user.click(screen.getByRole("combobox", { name: "Versions" }));
  await user.click(await screen.findByRole("option", { name: "Edition two" }));
  await screen.findByRole("alertdialog", { name: "Leave this version?" });
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  expect(screen.getByLabelText("Editor URL")).toHaveTextContent("VERSION1");
  expect(title).toHaveValue("Old title unsaved");
  await user.click(screen.getByRole("combobox", { name: "Versions" }));
  await user.click(await screen.findByRole("option", { name: "Edition two" }));
  await user.click(await screen.findByRole("button", { name: "Discard changes" }));
  await waitFor(() => expect(screen.getByLabelText("Editor URL")).toHaveTextContent("VERSION2"));
  expect(await screen.findByText("Actor two")).toBeVisible();
  expect(screen.queryByText("Actor one")).toBeNull();
  expect(screen.getByLabelText("Title")).toHaveValue("Old title");
  expect(writes(s)).toEqual([]);
});


it("never saves vanished version credits into the first remaining version", async () => {
  const s = server({ model: scopedModel, versions: [edition, otherEdition], memberships: Promise.resolve(scopedLinks) });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  await user.click(await screen.findByRole("button", { name: "Remove Actor two" }));
  s.item.versions = [edition];
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  expect(await screen.findByText("This version could not be found.")).toBeVisible();
  expect(writes(s)).toEqual([]);
});

it("shows a shared original title and the selected edition's display title without another language picker", async () => {
  const titled: ContentModel = { ...model, kinds: { ...model.kinds, gallery: { ...model.kinds.gallery!, title_policy: "original_and_version" } } };
  const s = server({ model: titled, versions: [
    { ...edition, text: { en: { title: "First edition" } } },
    { ...otherEdition, languages: ["en"], text: { en: { title: "RESORT LOVER" } } },
  ] });
  s.item.original_title = "原題";
  s.item.original_language = "ja";
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  expect(await screen.findByLabelText("Original title")).toHaveValue("原題");
  expect(screen.getByLabelText("Display title (en)")).toHaveValue("RESORT LOVER");
  expect(screen.queryByRole("combobox", { name: "Language" })).toBeNull();
  expect(writes(s)).toEqual([]);
});

it("saves original and edition titles to their respective scopes without overwriting sibling titles", async () => {
  const titled: ContentModel = { ...model, kinds: { ...model.kinds, gallery: { ...model.kinds.gallery!, title_policy: "original_and_version" } } };
  const s = server({ model: titled, versions: [
    { ...edition, text: { en: { title: "First edition" } } },
    { ...otherEdition, languages: ["en"], text: { en: { title: "RESORT LOVER", description: "Retain this" }, ja: { title: "既存の訳" } } },
  ] });
  s.item.original_title = "原題";
  s.item.original_language = "ja";
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION2" />);
  const original = await screen.findByLabelText("Original title");
  await user.clear(original);
  await user.type(original, "新しい原題");
  const display = screen.getByLabelText("Display title (en)");
  await user.clear(display);
  await user.type(display, "Resort Lover revised");
  expect(writes(s)).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await screen.findByText("Saved.", { selector: "p[role=status]" });
  expect(s.item.original_title).toBe("新しい原題");
  expect(s.item.original_language).toBe("ja");
  expect(s.item.versions[0]?.text?.en?.title).toBe("First edition");
  expect(s.item.versions[1]?.text).toEqual({ en: { title: "Resort Lover revised", description: "Retain this" }, ja: { title: "既存の訳" } });
  expect(s.item.text?.en?.title).toBe("Old title");
});

it("binds the display title to a staged edition language before one shared Save", async () => {
  const titled: ContentModel = { ...model, kinds: { ...model.kinds, gallery: { ...model.kinds.gallery!, title_policy: "original_and_version" } } };
  const s = server({ model: titled, versions: [
    { ...edition, text: { en: { title: "Keep English title" }, ja: { title: "元のタイトル" } } },
    { ...otherEdition, text: { en: { title: "Keep sibling title" } } },
  ] });
  const user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION1" />);
  expect(await screen.findByLabelText("Display title (en)")).toHaveValue("Keep English title");
  await user.click(screen.getByRole("combobox", { name: "Version language" }));
  await user.click(await screen.findByRole("option", { name: "Japanese" }));
  const display = await screen.findByLabelText("Display title (ja)");
  expect(display).toHaveValue("元のタイトル");
  await user.clear(display);
  await user.type(display, "新しいタイトル");
  expect(writes(s)).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await screen.findByText("Saved.", { selector: "p[role=status]" });
  expect(s.item.versions[0]?.languages).toEqual(["ja"]);
  expect(s.item.versions[0]?.text).toEqual({ en: { title: "Keep English title" }, ja: { title: "新しいタイトル" } });
  expect(s.item.versions[1]?.text).toEqual({ en: { title: "Keep sibling title" } });
});

it("stages continuous reading layout in Reader tools and preserves unrelated edition traits", async () => {
  const continuous: ContentModel = { ...model, kinds: { ...model.kinds, gallery: { ...model.kinds.gallery!, versions: { languages: "one", traits: ["manhwa_style", "preserve_trait"] } } } };
  const s = server({ model: continuous, versions: [{ ...edition, traits: ["preserve_trait"] }, otherEdition] }), user = userEvent.setup();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION1" />);
  await user.click(await screen.findByText("Reader tools"));
  const layout = await screen.findByRole("combobox", { name: "Reading layout" });
  expect(layout.closest('[data-ckui="version-reader-settings"]')).not.toBeNull();
  await user.click(layout);
  await user.click(await screen.findByRole("option", { name: "Continuous vertical" }));
  expect(writes(s)).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Save draft" }));
  await screen.findByText("Saved.", { selector: "p[role=status]" });
  expect(s.item.versions[0]?.traits).toEqual(["preserve_trait", "manhwa_style"]);
  expect(s.item.versions[1]?.traits).toEqual([]);
  expect(s.item.versions[0]?.state).toBe("published");
});

it("shows Audio as its own section beside Chapters for a kind with audio uploads, outside Reader tools", async () => {
  const media = read([{ ...page, page_id: "a".repeat(64) }, { path: "audio/001.mp3", type: "audio/mpeg", upload: true }]);
  const s = server({ media: { ...media, uploads: [...media.uploads!, { path: "audio/{name}", types: ["audio/mpeg"], max_bytes: 10_000_000 }] } });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION1" />);
  const audio = (await screen.findByRole("heading", { name: "Audio", level: 2 })).closest("section")!;
  expect(audio.closest("details")).toBeNull();
  expect(within(audio).getByText(/a cue plays one file when the reader reaches its page or anchor/)).toBeVisible();
  expect(within(audio).getByText(/An anchor marks a point partway down a page/)).toBeVisible();
  expect(within(audio).getByText(/Tracks on the same layer replace each other/)).toBeVisible();
  expect(await within(audio).findByText(/1 pages and 1 audio files/)).toBeVisible();
  expect(screen.queryByText("Reader tools")).toBeNull();
});

it("leaves the Audio section out for a kind without audio uploads", async () => {
  const s = server();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="VERSION1" />);
  expect(await screen.findByRole("heading", { name: "Chapters", level: 2 })).toBeVisible();
  expect(screen.queryByRole("heading", { name: "Audio" })).toBeNull();
});

it("deletes the last edition, opens it from Trash, and restores only that edition as a private draft", async () => {
  const direct: ContentModel = { ...model, kinds: { gallery: { ...model.kinds.gallery!, access_scope: "version" } } };
  const s = server({ model: direct }), user = userEvent.setup();
  function Route() {
    const [at, setAt] = useState({ pathname: "/galleries/GALLERY01/versions/VERSION1", search: "" });
    return <ContentManager client={s.client} kind="gallery" basePath="/galleries" navigation={{ ...at, navigate: (to) => setAt({ pathname: to.split("?")[0]!, search: to.includes("?") ? `?${to.split("?")[1]}` : "" }) }} />;
  }
  render(<Route />);
  await user.click(await screen.findByRole("button", { name: "Delete version" }));
  const confirmation = await screen.findByRole("alertdialog", { name: "Delete version" });
  await user.click(within(confirmation).getByRole("button", { name: "Delete version" }));
  await screen.findByRole("button", { name: "Restore version" });
  expect(s.item.state).toBe("deleted");
  await user.click(screen.getByRole("button", { name: "All items" }));
  await user.click(await screen.findByRole("button", { name: "Deleted" }));
  const manage = await screen.findByRole("button", { name: "Manage versions" });
  expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
  expect(screen.queryByRole("checkbox", { name: "Select every item on this page" })).toBeNull();
  await user.click(manage);
  const restore = await screen.findByRole("button", { name: "Restore version" });
  expect(restore).toBeEnabled();
  expect(restore.closest("fieldset[disabled]")).toBeNull();
  expect(screen.getByLabelText("Title")).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Save draft" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Delete version" })).toBeNull();
  await user.click(restore);
  await waitFor(() => expect(screen.getByLabelText("Title")).toBeEnabled());
  expect(s.item.versions[0]?.state).toBe("draft");
  expect(writes(s).map((call) => [call.method, call.path, call.body])).toEqual([
    ["DELETE", "/contents/c1/versions/v1", undefined],
    ["PATCH", "/contents/c1/versions/v1", { state: "draft" }],
  ]);
});

it("defaults to an undeleted edition even if a deleted edition has the first position", async () => {
  const s = server({ versions: [{ ...edition, state: "deleted" }, { ...otherEdition, position: 2 }] });
  const changed = vi.fn();
  render(<ContentEditor client={s.client} kind="gallery" id="c1" onVersionChange={changed} />);
  await waitFor(() => expect(changed).toHaveBeenCalled());
  expect(changed.mock.calls[0]?.[0]?.id).toBe("v2");
  expect(screen.queryByRole("button", { name: "Restore version" })).toBeNull();
});

it("keeps cover choices open during a pending copy and closes the actual crop dialog on Escape", async () => {
  const s = server(), user = userEvent.setup();
  vi.spyOn(s.client.media, "presetPath").mockResolvedValue("cover/i-pending.png");
  let finish!: (files: FileInfo[]) => void;
  const copied = new Promise<FileInfo[]>((resolve) => { finish = resolve; });
  const commit = vi.spyOn(s.client.media, "commit").mockReturnValue(copied);
  vi.spyOn(s.client.contents, "setCover").mockResolvedValue(edition);
  render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await user.click(await screen.findByRole("button", { name: "Reorder 001.png" }));
  await user.click(screen.getByRole("button", { name: "Thumbnail" }));
  await user.click(await screen.findByRole("button", { name: "Set as thumbnail" }));
  await waitFor(() => expect(commit).toHaveBeenCalledOnce());
  await user.keyboard("{Escape}");
  expect(screen.getByRole("dialog")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
  expect(document.querySelector('[data-ckui="crop-dialog"]')).toBeNull();
  await act(async () => { finish([]); await copied; });
  await waitFor(() => expect(document.querySelector('[data-ckui="crop-dialog"]')).not.toBeNull());
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled();
});

it.each([
  { editor_url: "https://test.invalid/private-edited-cover.webp", expected: "https://test.invalid/private-edited-cover.webp" },
  { editor_url: undefined, expected: "https://test.invalid/private-original-cover.png" },
])("shows the selected private draft header cover after its authorized media arrives ($expected)", async ({ editor_url, expected }) => {
  const selected: Version = { ...edition, state: "draft", cover: { image: "i-selected", url: "" } };
  const s = server({ versions: [selected] });
  let finish!: (value: ReadResult) => void;
  s.media.mockReturnValue(new Promise<ReadResult>((resolve) => { finish = resolve; }));
  const view = render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  const header = view.container.querySelector('[data-ckui="editor-thumbnail"]')!;
  expect(header.querySelector("img")).toBeNull();
  await act(async () => finish(read([
    page,
    { path: "cover/i-other.png", type: "image/png", upload: true, url: "https://test.invalid/wrong-cover.png" },
    { path: "cover/i-selected.png", type: "image/png", upload: true, url: "https://test.invalid/private-original-cover.png", editor_url },
  ])));
  await waitFor(() => expect(header.querySelector("img")).toHaveAttribute("src", expected));
});

it("prefers the published header cover URL over authorized editor previews", async () => {
  const published: Version = { ...edition, cover: { image: "i-selected", url: "https://test.invalid/public-cover.webp" } };
  const s = server({ versions: [published], media: read([
    page,
    { path: "cover/i-selected.png", type: "image/png", upload: true, url: "https://test.invalid/private-original-cover.png", editor_url: "https://test.invalid/private-edited-cover.webp" },
  ]) });
  const view = render(<ContentEditor client={s.client} kind="gallery" id="c1" />);
  await screen.findByLabelText("Title");
  await waitFor(() => expect(view.container.querySelector('[data-ckui="editor-thumbnail"] img')).toHaveAttribute("src", "https://test.invalid/public-cover.webp"));
});

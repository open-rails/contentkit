// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, EntityCard, EntityDetail, Version } from "../../client/generated/wire.js";
import { toLocalInput } from "../../client/entity-fields.js";
import { ContentKitProvider } from "../../react/index.js";
import { AccessEditor, SegmentsEditor, VersionEditor, VersionsEditor } from "./editors.js";

type Handler = (req: { url: URL; body: unknown }) => unknown;

function server(routes: Record<string, Handler>) {
  const calls: { key: string; body: unknown }[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://x");
    const key = `${init?.method ?? "GET"} ${url.pathname.replace(/^\/v1/, "")}`;
    const body = init?.body && typeof init.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push({ key, body });
    const h = routes[key];
    if (!h) return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
    return Response.json(await h({ url, body }));
  });
  return { calls, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer="creator">
        {children}
      </ContentKitProvider>
    );
  };

const model: ContentModel = {
  languages: ["en"],
  tiers: [],
  signals: false,
  kinds: {
    post: { route: "p", presentation: "post", versions: { languages: "one", hidden: true } },
    comic: { route: "c", presentation: "reader", versions: { languages: "one" }, segments: true },
  },
  entity_kinds: { channel: { owner: "user", entitled: true, auth_scope: true } },
};

const at = "2026-10-13T00:00:00Z";
const version: Version = { id: "v1", code: "AB12", position: 1, languages: ["en"], traits: [], live: false, has_audio: false,
  entities: [], attributes: {}, segments: [], decision: { allowed: true } };
const content = (o: Partial<Content> = {}): Content => ({
  id: "p1",
  kind: "post",
  code: "ABCDEFGH1",
  path: "/p/ABCDEFGH1",
  language: "en",
  title: "A post",
  decision: { allowed: true },
  state: "draft",
  comments: "open",
  live: false,
  access: { level: "public" },
  restrictions: [],
  attributes: {},
  creators: [],
  entities: [],
  versions: [version],
  editor: true,
  revision: 3,
  created_at: at,
  updated_at: at,
  ...o,
});
const card = (id: string, name: string): EntityCard => ({ id, internal: false, kind: "channel", slug: name.toLowerCase(), name, count: 0, attributes: {}, created_at: at });
const detail = (id: string, name: string): EntityDetail => ({
  ...card(id, name),
  state: "active",
  updated_at: at,
  revision: 1,
  names: [],
  text: [],
  children: [],
  edges: [],
  counts: [],
});

it("VersionsEditor: a kind whose one version is hidden keeps that version's media upload, without version management", async () => {
  const s = server({ "GET /kinds": () => model });
  // The flow's client, no provider, as ContentManager renders it.
  const { container } = render(<VersionsEditor content={content()} client={s.client} />);
  await waitFor(() => expect(container.querySelector('[data-ckui="version-media"] [data-ckui="media-folder"]')).not.toBeNull());
  expect(container.querySelector('[data-ckui="versions-editor"]')).toBeNull();
  expect(screen.queryByRole("button", { name: "Add version" })).toBeNull();
});

it("AccessEditor: members-only defaults to the owner group, and the picker chooses another", async () => {
  const s = server({
    "GET /kinds": () => model,
    "GET /entities/g-own": () => detail("g-own", "Studio"),
    "GET /entities/g-other": () => detail("g-other", "Other Club"),
    "GET /typeahead": () => ({ contents: [], entities: [card("g-other", "Other Club")] }),
    "PATCH /contents/p1": ({ body }) => content({ access: (body as { access: Content["access"] }).access, revision: 4 }),
  });
  const user = userEvent.setup();
  const { container } = render(<AccessEditor content={content({ owner_group_id: "g-own" })} />, { wrapper: wrap(s.client) });
  const editor = container.querySelector<HTMLElement>('[data-ckui="access-editor"]')!;

  await user.click(await within(editor).findByRole("combobox", { name: "Who can open it" }));
  await user.click(await screen.findByRole("option", { name: "Entity members" }));
  expect(await within(editor).findByText("Studio")).toBeInTheDocument();
  // A post has neither pages nor chapters to preview.
  expect(within(editor).queryByLabelText("Free pages")).toBeNull();
  expect(within(editor).queryByLabelText("Free chapters")).toBeNull();
  expect(within(editor).getByRole("button", { name: "Add audience release" })).toBeInTheDocument();
  expect(within(editor).queryByLabelText("Release 1 date")).toBeNull();

  await user.click(within(editor).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.find((c) => c.key === "PATCH /contents/p1")?.body).toEqual({ access: { level: "group", group_id: "g-own" }, revision: 3 }));

  await user.type(within(editor).getByRole("combobox", { name: "Entity" }), "oth");
  await user.click(await screen.findByRole("option", { name: /Other Club/ }));
  expect(await within(editor).findByText("Other Club")).toBeInTheDocument();
  await user.click(within(editor).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.filter((c) => c.key === "PATCH /contents/p1").at(-1)?.body).toMatchObject({ access: { level: "group", group_id: "g-other" } }));
});

it("AccessEditor: a group level saves only with a group; previews are the kind's own", async () => {
  const s = server({ "GET /kinds": () => model });
  const user = userEvent.setup();
  const { container } = render(<AccessEditor content={content({ id: "c1", kind: "comic" })} />, { wrapper: wrap(s.client) });
  const editor = container.querySelector<HTMLElement>('[data-ckui="access-editor"]')!;
  await user.click(await within(editor).findByRole("combobox", { name: "Who can open it" }));
  await user.click(await screen.findByRole("option", { name: "Entity members" }));
  expect(within(editor).getByRole("button", { name: "Save" })).toBeDisabled();
  expect(within(editor).getByRole("combobox", { name: "Entity" })).toBeInTheDocument();
  expect(within(editor).getByLabelText("Free pages")).toBeInTheDocument();
  expect(within(editor).getByLabelText("Free chapters")).toBeInTheDocument();
});


it("subscription-only policy shows two access choices, saves its single tier, and removes all offer fields", async () => {
  const limited = { ...model, tiers: ["premium"], kinds: { ...model.kinds, comic: { ...model.kinds.comic!, access_levels: ["public", "tier"] } } };
  const s = server({
    "GET /kinds": () => limited,
    "PATCH /contents/p1": ({ body }) => content({ kind: "comic", access: (body as { access: Content["access"] }).access, revision: 4 }),
  });
  const user = userEvent.setup();
  const { container } = render(<AccessEditor content={content({ kind: "comic" })} />, { wrapper: wrap(s.client) });
  expect(await screen.findByRole("heading", { name: "Access" })).toBeVisible();
  await user.click(screen.getByRole("combobox", { name: "Who can open it" }));
  expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Everyone", "Tier members"]);
  await user.click(await screen.findByRole("option", { name: "Tier members" }));
  expect(screen.queryByRole("switch", { name: "premium" })).toBeNull();
  expect(container.querySelector('[data-ckui="offer-fields"]')).toBeNull();
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.find((c) => c.key === "PATCH /contents/p1")?.body).toEqual({ access: { level: "tier", tiers: ["premium"] }, revision: 3 }));
});

it("subscription-only versions do not expose prices or purchases", async () => {
  const limited = { ...model, tiers: ["premium"], kinds: { ...model.kinds, comic: { ...model.kinds.comic!, access_levels: ["public", "tier"] } } };
  const s = server({ "GET /kinds": () => limited });
  const user = userEvent.setup();
  const { container } = render(<VersionsEditor content={content({ kind: "comic" })} media={false} client={s.client} />);
  await user.click(await screen.findByRole("button", { name: "Edit" }));
  expect(container.querySelector('[data-ckui="offer-fields"]')).toBeNull();
  expect(screen.queryByLabelText("Price")).toBeNull();
  await user.click(screen.getByRole("switch", { name: "Its own access rule" }));
  await user.click(screen.getByRole("combobox", { name: "Who can open it" }));
  expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Everyone", "Tier members"]);
});

it("edits one selected version without duplicating its media or chapter panels", async () => {
  const limited = { ...model, kinds: { ...model.kinds, comic: { ...model.kinds.comic!, access_levels: ["public", "tier"] } } };
  const s = server({
    "GET /kinds": () => limited,
    "PATCH /contents/p1/versions/v1": ({ body }) => ({ ...version, ...body as object }),
  });
  const user = userEvent.setup();
  const { container } = render(<VersionEditor content={content({ kind: "comic" })} version={version} media={false} chapters={false} client={s.client} />);
  fireEvent.change(await screen.findByLabelText("Goes live"), { target: { value: "2027-01-01T12:00" } });
  expect(screen.queryByLabelText("Label (optional)")).toBeNull();
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.find((call) => call.key === "PATCH /contents/p1/versions/v1")?.body).toMatchObject({ publish_at: new Date("2027-01-01T12:00").toISOString(), languages: ["en"] }));
  expect(container.querySelector('[data-ckui="segments-editor"]')).toBeNull();
  expect(container.querySelector('[data-ckui="media-folder"]')).toBeNull();
  expect(screen.queryByRole("button", { name: "Add version" })).toBeNull();
});

it("notifies the host only after the selected version is successfully deleted", async () => {
  let rejected = true;
  const s = server({
    "GET /kinds": () => model,
    "DELETE /contents/p1/versions/v1": () => {
      if (rejected) throw new Error("Delete refused");
      return null;
    },
  });
  const onDeleted = vi.fn(), onError = vi.fn(), user = userEvent.setup();
  render(<VersionEditor content={content()} version={version} media={false} chapters={false} client={s.client} onDeleted={onDeleted} onError={onError} />);
  const remove = await screen.findByRole("button", { name: "Delete version" });
  await user.click(remove);
  await waitFor(() => expect(onError).toHaveBeenCalledOnce());
  expect(onDeleted).not.toHaveBeenCalled();
  rejected = false;
  await user.click(remove);
  await waitFor(() => expect(onDeleted).toHaveBeenCalledExactlyOnceWith(version));
});

it("lets the host cancel edition deletion to preserve unsaved parent fields", async () => {
  const s = server({ "GET /kinds": () => model, "DELETE /contents/p1/versions/v1": () => null });
  const beforeDelete = vi.fn<() => boolean | Promise<boolean>>().mockReturnValueOnce(false).mockResolvedValueOnce(true);
  const onDeleted = vi.fn(), user = userEvent.setup();
  render(<VersionEditor content={content()} version={version} media={false} chapters={false} client={s.client} beforeDelete={beforeDelete} onDeleted={onDeleted} />);
  const remove = await screen.findByRole("button", { name: "Delete version" });
  await user.click(remove);
  expect(beforeDelete).toHaveBeenCalledOnce();
  expect(s.calls.some((call) => call.key.startsWith("DELETE"))).toBe(false);
  expect(onDeleted).not.toHaveBeenCalled();
  await user.click(remove);
  await waitFor(() => expect(onDeleted).toHaveBeenCalledExactlyOnceWith(version));
  expect(s.calls.filter((call) => call.key.startsWith("DELETE"))).toHaveLength(1);
});

it("resets unsaved version fields when selecting another edition", async () => {
  const s = server({ "GET /kinds": () => model });
  const view = render(<VersionEditor content={content()} version={version} media={false} chapters={false} client={s.client} />);
  fireEvent.change(await screen.findByLabelText("Goes live"), { target: { value: "2027-01-01T12:00" } });
  const second = { ...version, id: "v2", code: "SECOND", publish_at: "2028-01-01T12:00:00Z" };
  view.rerender(<VersionEditor content={content()} version={second} media={false} chapters={false} client={s.client} />);
  expect(screen.getByLabelText("Goes live")).toHaveValue(toLocalInput(second.publish_at));
});

it("can render chapter controls under a host-supplied heading", async () => {
  const s = server({ "GET /kinds": () => model });
  render(<SegmentsEditor content={content({ kind: "comic" })} version={version} heading={false} client={s.client} />);
  expect(screen.queryByRole("heading", { name: "Chapters" })).toBeNull();
  expect(await screen.findByRole("button", { name: "Add chapter" })).toBeVisible();
});

it.each([
  ["draft", "Publish", "published"],
  ["published", "Unpublish", "draft"],
] as const)("standalone version %s saves staged fields and %s in one edition-only write", async (state, action, nextState) => {
  const s = server({
    "GET /kinds": () => model,
    "PATCH /contents/p1/versions/v1": ({ body }) => ({ ...version, ...body as object }),
  });
  const user = userEvent.setup();
  render(<VersionEditor content={content()} version={{ ...version, state }} media={false} chapters={false} client={s.client} />);
  fireEvent.change(await screen.findByLabelText("Goes live"), { target: { value: "2027-01-01T12:00" } });
  expect(s.calls.some((call) => call.key.startsWith("PATCH"))).toBe(false);
  await user.click(screen.getByRole("button", { name: action }));
  await waitFor(() => expect(s.calls.filter((call) => call.key.startsWith("PATCH"))).toHaveLength(1));
  expect(s.calls.find((call) => call.key.startsWith("PATCH"))).toMatchObject({
    key: "PATCH /contents/p1/versions/v1",
    body: { state: nextState, languages: ["en"], publish_at: new Date("2027-01-01T12:00").toISOString() },
  });
  expect(screen.queryByRole("switch", { name: "Draft" })).toBeNull();
});

it("a deleted standalone edition exposes only Restore and never restores directly to published", async () => {
  const s = server({ "GET /kinds": () => model, "PATCH /contents/p1/versions/v1": ({ body }) => ({ ...version, ...body as object }) });
  const user = userEvent.setup(), restored = vi.fn();
  render(<VersionEditor content={content()} version={{ ...version, state: "deleted" }} media={false} chapters={false} client={s.client} onRestored={restored} />);
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Delete version" })).toBeNull();
  await user.click(await screen.findByRole("button", { name: "Restore version" }));
  await waitFor(() => expect(restored).toHaveBeenCalledOnce());
  expect(s.calls.filter((call) => call.key.startsWith("PATCH"))).toEqual([{ key: "PATCH /contents/p1/versions/v1", body: { state: "draft" } }]);
});

it("reorders only undeleted editions while retaining deleted-edition restoration", async () => {
  const first = { ...version, label: "First", position: 1, state: "published" as const };
  const deleted = { ...version, id: "deleted", code: "DELETED", label: "Deleted edition", position: 2, state: "deleted" as const };
  const second = { ...version, id: "second", code: "SECOND", label: "Second", position: 3, state: "published" as const };
  const parent = content({ kind: "comic", versions: [first, deleted, second] });
  const s = server({ "GET /kinds": () => model, "PUT /contents/p1/versions/order": () => parent });
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    const row = this.closest("[data-overlay]") ? document.querySelector("li[data-dragging]") : this.closest("li");
    const index = row?.parentElement ? [...row.parentElement.children].indexOf(row) : 0;
    return { x: 0, y: index * 40, top: index * 40, left: 0, bottom: index * 40 + 36, right: 300, width: 300, height: 36, toJSON: () => ({}) } as DOMRect;
  });
  try {
    const { container } = render(<VersionsEditor content={parent} client={s.client} media={false} />);
    const handle = await screen.findByRole("button", { name: "Reorder First" });
    const tombstone = container.querySelector<HTMLElement>('[data-ckui="deleted-version"]')!;
    expect(tombstone).not.toBeNull();
    expect(tombstone.querySelector('[data-ckui="sort-handle"]')).toBeNull();
    handle.focus();
    const tick = () => act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    fireEvent.keyDown(handle, { key: " ", code: "Space" });
    await tick();
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown", code: "ArrowDown" });
    await tick();
    fireEvent.keyDown(document.activeElement!, { key: " ", code: "Space" });
    await waitFor(() => expect(s.calls.find((call) => call.key === "PUT /contents/p1/versions/order")?.body).toEqual({ ids: ["second", "v1"] }));
    await userEvent.click(within(tombstone).getByRole("button", { name: "Edit" }));
    expect(within(tombstone).getByRole("button", { name: "Restore version" })).toBeVisible();
  } finally {
    rect.mockRestore();
  }
});

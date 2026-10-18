// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, GroupCard, GroupDetail, Version } from "../../client/generated/wire.js";
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
  entity_kinds: { channel: { owner: "user", sellable: true, auth_scope: true } },
};

const at = "2026-10-13T00:00:00Z";
const version: Version = { id: "v1", code: "AB12", position: 1, languages: ["en"], traits: [], live: false, has_audio: false, attributes: {}, segments: [], decision: { allowed: true } };
const content = (o: Partial<Content> = {}): Content => ({
  id: "p1",
  kind: "post",
  code: "ABCDEFGH1",
  path: "/p/ABCDEFGH1",
  language: "en",
  title: "A post",
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
  versions: [version],
  editor: true,
  revision: 3,
  created_at: at,
  updated_at: at,
  ...o,
});
const card = (id: string, name: string): GroupCard => ({ id, internal: false, kind: "channel", slug: name.toLowerCase(), name, count: 0, attributes: {}, created_at: at });
const detail = (id: string, name: string): GroupDetail => ({
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
  await user.click(await screen.findByRole("option", { name: "Group members" }));
  expect(await within(editor).findByText("Studio")).toBeInTheDocument();
  // A post has neither pages nor chapters to preview.
  expect(within(editor).queryByLabelText("Free pages")).toBeNull();
  expect(within(editor).queryByLabelText("Free chapters")).toBeNull();
  expect(within(editor).getByLabelText("Free for everyone from")).toBeInTheDocument();

  await user.click(within(editor).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.find((c) => c.key === "PATCH /contents/p1")?.body).toEqual({ access: { level: "group", group_id: "g-own" }, revision: 3 }));

  await user.type(within(editor).getByRole("combobox", { name: "Group" }), "oth");
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
  await user.click(await screen.findByRole("option", { name: "Group members" }));
  expect(within(editor).getByRole("button", { name: "Save" })).toBeDisabled();
  expect(within(editor).getByRole("combobox", { name: "Group" })).toBeInTheDocument();
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
  const label = await screen.findByLabelText("Label (optional)");
  await user.type(label, "Revised edition");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls.find((call) => call.key === "PATCH /contents/p1/versions/v1")?.body).toMatchObject({ label: "Revised edition", languages: ["en"] }));
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
  const s = server({ "GET /kinds": () => model }), user = userEvent.setup();
  const view = render(<VersionEditor content={content()} version={version} media={false} chapters={false} client={s.client} />);
  await user.type(await screen.findByLabelText("Label (optional)"), "First draft");
  const second = { ...version, id: "v2", code: "SECOND", label: "Second edition" };
  view.rerender(<VersionEditor content={content()} version={second} media={false} chapters={false} client={s.client} />);
  expect(screen.getByLabelText("Label (optional)")).toHaveValue("Second edition");
});

it("can render chapter controls under a host-supplied heading", async () => {
  const s = server({ "GET /kinds": () => model });
  render(<SegmentsEditor content={content({ kind: "comic" })} version={version} heading={false} client={s.client} />);
  expect(screen.queryByRole("heading", { name: "Chapters" })).toBeNull();
  expect(await screen.findByRole("button", { name: "Add chapter" })).toBeVisible();
});

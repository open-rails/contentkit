// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, GroupCard, GroupDetail, Version } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { AccessEditor, VersionsEditor } from "./editors.js";

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
  content_filters: [],
  signals: false,
  kinds: {
    post: { route: "p", presentation: "post", versions: { languages: "one", hidden: true } },
    comic: { route: "c", presentation: "reader", versions: { languages: "one" }, segments: true },
  },
  group_kinds: { channel: { owner: "user", sellable: true, auth_scope: true } },
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
  views: 0,
  decision: { allowed: true },
  state: "draft",
  listing: "listed",
  comments: "open",
  live: false,
  access: { level: "public" },
  restrictions: [],
  attributes: {},
  creators: [],
  groups: [],
  versions: [version],
  editor: true,
  revision: 3,
  created_at: at,
  updated_at: at,
  ...o,
});
const card = (id: string, name: string): GroupCard => ({ id, kind: "channel", slug: name.toLowerCase(), name, count: 0, attributes: {}, created_at: at });
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
    "GET /groups/g-own": () => detail("g-own", "Studio"),
    "GET /groups/g-other": () => detail("g-other", "Other Club"),
    "GET /typeahead": () => ({ contents: [], groups: [card("g-other", "Other Club")] }),
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

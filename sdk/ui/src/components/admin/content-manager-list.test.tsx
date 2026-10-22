// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, ManageCount, Rule, Version } from "../../client/generated/wire.js";
import { ContentEditor, ContentManager } from "./content-manager.js";

const model: ContentModel = {
  languages: ["en"],
  tiers: ["premium"],
  signals: false,
  kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" }, entities: [], segments: true } },
  entity_kinds: {},
};
const at = "2026-10-16T00:00:00Z";
const item = (id: string, title: string, access: Rule, extra: Partial<Content> = {}): Content => ({
  id,
  code: `CODE${id.toUpperCase()}`,
  kind: "gallery",
  path: `/g/CODE${id}`,
  language: "en",
  title,
  decision: { allowed: true },
  state: "published",
  published_at: at,
  comments: "open",
  live: true,
  access,
  restrictions: [],
  attributes: {},
  creators: [],
  entities: [],
  versions: [],
  editor: true,
  revision: 1,
  created_at: at,
  updated_at: at,
  ...extra,
});

function server(items: Content[], o: { count?: ManageCount; fail?: string; model?: ContentModel } = {}) {
  const calls: { method: string; path: string; query: URLSearchParams; body: unknown }[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    const path = url.pathname.replace(/^\/v1/, "");
    const method = init?.method ?? "GET";
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    calls.push({ method, path, query: url.searchParams, body });
    if (method === "GET" && path === "/kinds") return Response.json(o.model ?? model);
    const shown = url.searchParams.get("q") ? [] : items;
    if (method === "GET" && path === "/manage/gallery") return Response.json(shown);
    if (method === "GET" && path === "/manage/gallery/count") return Response.json(o.count ?? { total: shown.length });
    const patch = /^\/contents\/(\w+)$/.exec(path);
    if (method === "GET" && patch) return Response.json(items.find((c) => c.id === patch[1]));
    if (method === "PATCH" && patch) {
      if (patch[1] === o.fail) return Response.json({ code: "conflict", error: "Changed elsewhere" }, { status: 409 });
      const found = items.find((c) => c.id === patch[1])!;
      return Response.json({ ...found, ...body });
    }
    return Response.json({ code: "not_found", error: `Unexpected ${method} ${path}` }, { status: 404 });
  });
  return { client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }), calls };
}

function Route({ client, search = "", noun }: { client: ReturnType<typeof server>["client"]; search?: string; noun?: { one: string; other: string } }) {
  const [at, setAt] = useState({ pathname: "/galleries", search });
  return (
    <ContentManager
      client={client}
      kind="gallery"
      basePath="/galleries"
      noun={noun}
      navigation={{ ...at, navigate: (to) => setAt({ pathname: to.split("?")[0]!, search: to.includes("?") ? `?${to.split("?")[1]}` : "" }) }}
    />
  );
}

it("shows each row's cover, code, access and live date, and toggles premium in one click", async () => {
  const s = server([item("a", "Alpha", { level: "public" }, { cover: { url: "https://test.invalid/a.webp" } }), item("b", "Beta", { level: "tier", tiers: ["premium"] })]);
  const { container } = render(<Route client={s.client} />);
  const alpha = (await screen.findByText("Alpha")).closest("li")!;
  expect(within(alpha).getByText("CODEA")).toBeVisible();
  expect(within(alpha).getByText("Free")).toBeVisible();
  expect(within(alpha).getByText(/^Live /)).toBeVisible();
  expect(container.querySelector('img[src="https://test.invalid/a.webp"]')).not.toBeNull();
  expect(within(screen.getByText("Beta").closest("li")!).getByText("Premium")).toBeVisible();

  await userEvent.click(within(alpha).getByRole("button", { name: "Make premium" }));
  await waitFor(() => expect(s.calls.some((c) => c.method === "PATCH")).toBe(true));
  expect(s.calls.find((c) => c.method === "PATCH")).toMatchObject({ path: "/contents/a", body: { access: { level: "tier", tiers: ["premium"] } } });
});

it("filters by access on the server and counts the selection", async () => {
  const s = server([item("a", "Alpha", { level: "public" })], { count: { total: 10_000, truncated: true } });
  render(<Route client={s.client} search="?access=tier%3Apremium" />);
  await screen.findByText("Alpha");
  const list = s.calls.find((c) => c.path === "/manage/gallery")!;
  expect(list.query.get("access")).toBe("tier");
  expect(list.query.get("tier")).toBe("premium");
  expect(s.calls.find((c) => c.path === "/manage/gallery/count")!.query.get("tier")).toBe("premium");
  expect(await screen.findByText(/^Showing 1–20 of 10,?000\+$/)).toBeVisible();
  // A truncated total has no known last page.
  expect(screen.getByRole("button", { name: "Last" })).toBeDisabled();
});

it("bulk-updates the selection and reports partial failures", async () => {
  const s = server([item("a", "Alpha", { level: "tier", tiers: ["premium"] }), item("b", "Beta", { level: "tier", tiers: ["premium"] })], { fail: "b" });
  const user = userEvent.setup();
  render(<Route client={s.client} />);
  await screen.findByText("Alpha");
  await user.click(screen.getByRole("checkbox", { name: "Select every item on this page" }));
  expect(screen.getByText("2 selected")).toBeVisible();
  await user.click(within(screen.getByText("2 selected").parentElement!).getByRole("button", { name: "Make free" }));
  expect(await screen.findByText("1 of 2 updated; 1 failed and are unchanged.")).toBeVisible();
  expect(s.calls.filter((c) => c.method === "PATCH").map((c) => c.body)).toEqual([{ access: { level: "public" } }, { access: { level: "public" } }]);
  expect(screen.queryByText("2 selected")).not.toBeInTheDocument();
});

it("searches as the admin types and tells filtered from truly empty", async () => {
  const empty = server([]);
  const { unmount } = render(<Route client={empty.client} />);
  expect(await screen.findByText("No items yet.")).toBeVisible();
  // The empty state repeats the header's create action next to its message.
  expect(screen.getAllByRole("button", { name: "New item" })).toHaveLength(2);
  unmount();

  // A host-supplied kind name replaces the generic wording.
  const named = render(<Route client={server([]).client} noun={{ one: "gallery", other: "galleries" }} />);
  expect(await screen.findByText("No galleries yet.")).toBeVisible();
  expect(screen.getByRole("heading", { name: "galleries" })).toBeVisible();
  expect(screen.getAllByRole("button", { name: "New gallery" })).toHaveLength(2);
  named.unmount();

  const s = server([item("a", "Alpha", { level: "public" })]);
  const user = userEvent.setup();
  render(<Route client={s.client} />);
  await screen.findByText("Alpha");
  await user.type(screen.getByRole("searchbox"), "zz");
  expect(await screen.findByText("No items match these filters.")).toBeVisible();
  expect(s.calls.some((c) => c.path === "/manage/gallery" && c.query.get("q") === "zz")).toBe(true);
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  expect(await screen.findByText("Alpha")).toBeVisible();
});


it("does not advertise individual purchases in a subscription-only manager", async () => {
  const limited: ContentModel = { ...model, kinds: { gallery: { ...model.kinds.gallery!, access_levels: ["public", "tier"] } } };
  const s = server([], { model: limited });
  const user = userEvent.setup();
  render(<Route client={s.client} />);
  await user.click(await screen.findByRole("combobox", { name: "Access" }));
  expect(screen.queryByRole("option", { name: "Buyers" })).toBeNull();
  expect((await screen.findAllByRole("option")).map((o) => o.textContent)).not.toContain("Pay per view");
});

it("directs version-scoped gallery changes to the edition editor instead of parent bulk actions", async () => {
  const versionScoped: ContentModel = { ...model, kinds: { gallery: { ...model.kinds.gallery!, access_scope: "version" } } };
  const edition = (id: string, access: Rule): Version => ({ id, code: id.toUpperCase(), position: id === "free" ? 0 : 1, languages: ["en"], traits: [], state: "published", live: true, has_audio: false, entities: [], attributes: {}, segments: [], decision: { allowed: true }, access });
  const s = server([item("a", "Alpha", { level: "public" }, { versions: [edition("free", { level: "public" }), edition("paid", { level: "tier", tiers: ["premium"] })] })], { model: versionScoped });
  const { container } = render(<Route client={s.client} search="?access=public" />);
  const row = (await screen.findByText("Alpha")).closest("li")!;
  expect(within(row).getByRole("button", { name: "Edit" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Make premium" })).toBeNull();
  expect(screen.queryByRole("checkbox", { name: "Select every item on this page" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Access" })).toBeNull();
  expect(container.querySelector('[data-ckui="access-badge"]')).toBeNull();
  await waitFor(() => expect(s.calls.filter((call) => call.path === "/manage/gallery").at(-1)?.query.get("access")).toBeNull());
  expect(s.calls.some((call) => call.method !== "GET")).toBe(false);
});

it("visible-version players hide parent Delete in both the list and generic editor", async () => {
  const player: ContentModel = { ...model, kinds: { gallery: { ...model.kinds.gallery!, presentation: "player", access_scope: "version" } } };
  const s = server([item("a", "Alpha", { level: "public" })], { model: player });
  const list = render(<Route client={s.client} />);
  await screen.findByText("Alpha");
  expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  list.unmount();
  render(<ContentEditor id="a" kind="gallery" client={s.client} />);
  await screen.findByRole("heading", { name: "Edit" });
  expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
});

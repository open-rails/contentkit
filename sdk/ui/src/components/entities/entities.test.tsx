// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { ContentModel, EntityAccess, EntityDetail } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { EntityManager } from "../admin/entity-manager.js";
import { MembershipBox } from "./banner.js";
import { EntityEditor } from "./editor.js";
import { EntityAvatar, EntityCover, EntityPicker } from "./parts.js";

type Handler = (req: { url: URL; init?: RequestInit }) => unknown;

function server(routes: Record<string, Handler>) {
  const calls: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://x");
    const key = `${init?.method ?? "GET"} ${url.pathname.replace(/^\/v1/, "")}`;
    calls.push(key);
    const h = routes[key];
    if (!h) return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
    const out = await h({ url, init });
    return out instanceof Response ? out : Response.json(out);
  });
  return { calls, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer="fan">
        {children}
      </ContentKitProvider>
    );
  };

const key = "t:group:g1";
const entity = (access: EntityAccess) => ({ id: "g1", kind: "channel", name: "Studio", access });
it("MembershipBox delegates access to the host and never calls a ContentKit join route", async () => {
  const s = server({});
  const onCheckout = vi.fn();
  const user = userEvent.setup();
  const { rerender } = render(<MembershipBox entity={entity({ key, member: false })} onCheckout={onCheckout} />, { wrapper: wrap(s.client) });
  await user.click(screen.getByRole("button", { name: "Get access" }));
  expect(onCheckout).toHaveBeenCalledExactlyOnceWith({ key });
  expect(s.calls).toEqual([]);
  rerender(<MembershipBox entity={entity({ key, member: true })} onCheckout={onCheckout} />);
  expect(screen.getByText("Member")).toBeInTheDocument();
  expect(screen.queryByRole("button")).toBeNull();
});

it("MembershipBox does not invent an access action when the host has none", () => {
  const s = server({});
  const { container, rerender } = render(<MembershipBox entity={entity({ key, member: false })} />, { wrapper: wrap(s.client) });
  expect(container.querySelector('[data-ckui="membership-box"]')).toBeNull();
  rerender(<MembershipBox entity={entity({ key, member: false, unknown: true })} onCheckout={vi.fn()} />);
  expect(screen.getByText("We can't check your membership right now.")).toBeInTheDocument();
  expect(screen.queryByRole("button")).toBeNull();
});

const model: ContentModel = { languages: ["en"], tiers: [], signals: false, kinds: {}, entity_kinds: { channel: { owner: "user", entitled: true, auth_scope: true } } };
const at = "2026-10-13T00:00:00Z";
const detail: EntityDetail = {
  id: "g1",
  internal: false,
  kind: "channel",
  slug: "studio",
  state: "active",
  attributes: {},
  owner_user_id: "fan",
  created_at: at,
  updated_at: at,
  revision: 1,
  name: "Studio",
  names: [{ language: "en", kind: "name", name: "Studio" }],
  text: [],
  children: [],
  edges: [],
  counts: [],
  access: { key, member: false, editor: true },
};

it("EntityEditor contains no local membership price editor", async () => {
  const s = server({ "GET /kinds": () => model, "GET /entities/g1": () => detail });
  const { container } = render(<EntityEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
  await waitFor(() => expect(container.querySelector('[data-ckui="entity-text"]')).not.toBeNull());
  expect(container.querySelector('[data-ckui="entity-offer"]')).toBeNull();
  expect(screen.queryByLabelText("Price")).toBeNull();
});

it("EntityEditor keeps independent section state without duplicate sibling keys", async () => {
  const errors = vi.spyOn(console, "error").mockImplementation(() => {});
  try {
    const s = server({ "GET /kinds": () => model, "GET /entities/g1": () => detail });
    const { container } = render(<EntityEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
    await waitFor(() => expect(container.querySelector('[data-ckui="entity-text"]')).not.toBeNull());
    expect(errors.mock.calls.filter(args => String(args[0]).includes("same key"))).toEqual([]);
  } finally {
    errors.mockRestore();
  }
});

it("EntityAvatar and EntityCover are styling roots of their own outside a ContentKit surface", () => {
  const g = { id: "g1", kind: "channel", name: "Studio", slug: "studio" };
  const { container } = render(
    <div>
      <EntityAvatar entity={g} className="size-10" />
      <EntityCover entity={g} />
    </div>,
  );
  for (const slot of ["entity-avatar", "entity-cover"]) {
    const el = container.querySelector(`[data-ckui="${slot}"]`)!;
    expect(el).toHaveClass("ckui");
    expect(el).toHaveAttribute("data-ckui-theme");
  }
  expect(container.querySelector('[data-ckui="entity-avatar"]')).toHaveClass("rounded-full", "size-10");
});


it("staff entity editors read and update internal metadata through the management surface", async () => {
  let internal = true;
  const updates: unknown[] = [];
  const s = server({
    "GET /kinds": () => model,
    "GET /manage/entities/g1": () => ({ ...detail, internal }),
    "PATCH /entities/g1": ({ init }) => {
      const patch = JSON.parse(String(init?.body));
      updates.push(patch);
      internal = patch.internal;
      return { ...detail, internal };
    },
  });
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" management />, { wrapper: wrap(s.client) });
  const toggle = await screen.findByRole("switch", { name: "Internal use only" });
  expect(toggle).toBeChecked();
  expect(s.calls).toContain("GET /manage/entities/g1");
  expect(s.calls).not.toContain("GET /entities/g1");
  await user.click(toggle);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(updates).toHaveLength(1));
  expect(updates[0]).toMatchObject({ internal: false });
});

it("user-owned entity editors do not offer the staff-only internal setting", async () => {
  const s = server({ "GET /kinds": () => model, "GET /entities/g1": () => detail });
  render(<EntityEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
  await screen.findByRole("heading", { name: "Studio" });
  expect(screen.queryByRole("switch", { name: "Internal use only" })).toBeNull();
  expect(s.calls).not.toContain("GET /manage/entities/g1");
});

it("staff pickers find internal entities without making them public search suggestions", async () => {
  const queries: URL[] = [];
  const internal = { id: "private", kind: "tag", slug: "restricted", name: "Restricted", internal: true, count: 1 };
  const s = server({ "GET /manage/entities": ({ url }) => { queries.push(url); return { entities: [internal] }; } });
  const picked = vi.fn();
  const user = userEvent.setup();
  render(<EntityPicker kind="tag" label="Add tag" management onPick={picked} />, { wrapper: wrap(s.client) });
  await user.type(screen.getByRole("combobox", { name: "Add tag" }), "restr");
  await user.click(await screen.findByRole("option", { name: /Restricted/ }));
  expect(picked).toHaveBeenCalledWith(internal);
  expect(queries[0]?.searchParams.get("q")).toBe("restr");
  expect(queries[0]?.searchParams.get("kind")).toBe("tag");
  expect(s.calls).not.toContain("GET /typeahead");
});


it("does not retain internal suggestions when the signed-in account changes", async () => {
  let reply: ((value: unknown) => void) | undefined;
  let requests = 0;
  const s = server({ "GET /manage/entities": () => {
    if (++requests > 1) return Response.json({ code: "forbidden", error: "Forbidden" }, { status: 403 });
    return new Promise((resolve) => { reply = resolve; });
  } });
  const page = (viewer: string | null) => <ContentKitProvider client={s.client} viewer={viewer}><EntityPicker kind="tag" label="Private search" management onPick={vi.fn()} /></ContentKitProvider>;
  const view = render(page("staff"));
  await userEvent.setup().type(screen.getByRole("combobox", { name: "Private search" }), "restricted");
  await waitFor(() => expect(reply).toBeDefined());
  view.rerender(page(null));
  await act(async () => { reply!({ entities: [{ id: "hidden", kind: "tag", slug: "restricted", name: "Internal secret", internal: true }] }); });
  expect(screen.queryByRole("option", { name: /Internal secret/ })).toBeNull();
  await waitFor(() => expect(requests).toBe(2));
  expect(screen.queryByRole("option", { name: /Internal secret/ })).toBeNull();
});

const editorModel: ContentModel = {
  ...model,
  languages: ["en", "ja"],
  entity_kinds: { channel: { attributes: { rank: { type: "int" } } }, tag: {} },
};

function editableServer() {
  let current: EntityDetail = { ...detail, names: [...detail.names], text: [], attributes: {} };
  const patches: Record<string, unknown>[] = [];
  let fail = false;
  let wait: Promise<void> | undefined;
  const s = server({
    "GET /kinds": () => editorModel,
    "GET /entities/g1": () => current,
    "GET /manage/entities/g1": () => current,
    "GET /manage/entities": () => ({ entities: [{ ...current, count: 0 }] }),
    "GET /entities/g1/members": () => ({ members: [] }),
    "PATCH /entities/g1": async ({ init }) => {
      const patch = JSON.parse(String(init?.body));
      patches.push(patch);
      await wait;
      if (fail) return Response.json({ code: "conflict", error: "Conflict" }, { status: 409 });
      current = { ...current, ...patch, revision: current.revision + 1 };
      return current;
    },
  });
  return { ...s, patches, fail: (value: boolean) => { fail = value; }, pause: (value: Promise<void>) => { wait = value; }, set: (patch: Partial<EntityDetail>) => { current = { ...current, ...patch }; } };
}

it("saves details, staged aliases and descriptions in every language with one Save", async () => {
  const s = editableServer();
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" management />, { wrapper: wrap(s.client) });
  const slug = await screen.findByRole("textbox", { name: "Slug" });
  const save = screen.getByRole("button", { name: "Save" });
  expect(screen.getAllByRole("button", { name: "Save" })).toHaveLength(1);
  expect(save).toBeDisabled();
  await user.clear(slug);
  await user.type(slug, "new-studio");
  await user.type(screen.getByRole("spinbutton", { name: "Rank" }), "12");
  await user.click(screen.getByRole("switch", { name: "Internal use only" }));
  await user.type(screen.getByRole("textbox", { name: "A name or alias" }), "Alias one, Alias two");
  await user.click(screen.getByRole("button", { name: "Add" }));
  expect(screen.getByText("Alias one")).toBeInTheDocument();
  expect(s.calls.filter((call) => /^(PATCH|POST|PUT|DELETE) /.test(call))).toEqual([]);
  await user.type(screen.getByRole("textbox", { name: "Description (English)" }), "English description");
  await user.type(screen.getByRole("textbox", { name: "Description (Japanese)" }), "日本語の説明");
  await user.type(screen.getByRole("textbox", { name: "Sort as (Japanese)" }), "Studio JP");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  await user.click(save);
  await waitFor(() => expect(save).toBeDisabled());
  expect(s.patches).toEqual([{
    slug: "new-studio", internal: true, attributes: { rank: 12 },
    names: [...detail.names, { language: "en", kind: "alias", name: "Alias one" }, { language: "en", kind: "alias", name: "Alias two" }],
    text: [{ language: "en", description: "English description" }, { language: "ja", description: "日本語の説明", sort_key: "Studio JP" }],
  }]);
  expect(screen.getByRole("status")).toHaveTextContent("Saved.");
  const event = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
});

it("keeps all failed edits dirty and retries them together", async () => {
  const s = editableServer();
  s.fail(true);
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" management />, { wrapper: wrap(s.client) });
  const slug = await screen.findByRole("textbox", { name: "Slug" });
  await user.type(slug, "-edited");
  const input = screen.getByRole("textbox", { name: "A name or alias" });
  await user.type(input, "Unadded alias");
  const description = screen.getByRole("textbox", { name: "Description (Japanese)" });
  await user.type(description, "Pending description");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Could not save: Details, Names and aliases, Description. Your edits are kept.");
  expect(slug).toHaveValue("studio-edited");
  expect(input).toHaveValue("Unadded alias");
  expect(description).toHaveValue("Pending description");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  const event = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(true);
  s.fail(false);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Saved."));
  expect(s.patches).toHaveLength(2);
  expect(s.patches[1]).toEqual(s.patches[0]);
  expect(input).toHaveValue("");
  expect(screen.getByText("Unadded alias")).toBeInTheDocument();
});

it("stages canonical promotion and removal without saving and preserves other languages", async () => {
  const s = editableServer();
  const japanese = { language: "ja", kind: "name" as const, name: "スタジオ" };
  s.set({ names: [...detail.names, japanese, { language: "en", kind: "alias", name: "New name" }, { language: "en", kind: "alias", name: "Remove me" }] });
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
  await screen.findByRole("heading", { name: "Studio" });
  await user.click(screen.getByRole("button", { name: "Remove Remove me" }));
  await user.click(screen.getByRole("button", { name: "Make it the name" }));
  expect(s.patches).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Saved."));
  expect(s.patches).toEqual([{ names: [{ language: "en", kind: "alias", name: "Studio" }, japanese, { language: "en", kind: "name", name: "New name" }] }]);
});

it("protects edits made while saving and sends an empty array when all descriptions are cleared", async () => {
  const s = editableServer();
  s.set({ text: [{ language: "en", description: "Stored description" }] });
  let release!: () => void;
  s.pause(new Promise<void>((resolve) => { release = resolve; }));
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
  const description = await screen.findByRole("textbox", { name: "Description (English)" });
  await user.clear(description);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.patches).toEqual([{ text: [] }]));
  await user.type(description, "Typed during save");
  await act(async () => { release(); });
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  expect(description).toHaveValue("Typed during save");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Saved."));
  await user.clear(description);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Saved."));
  expect(s.patches).toEqual([{ text: [] }, { text: [{ language: "en", description: "Typed during save" }] }, { text: [] }]);
  expect(description).toHaveValue("");
});

it("clears the dirty state when edits are reverted and keeps edits through an immediate cover update", async () => {
  const s = editableServer();
  const user = userEvent.setup();
  render(<EntityEditor id="g1" kind="channel" management />, { wrapper: wrap(s.client) });
  const slug = await screen.findByRole("textbox", { name: "Slug" });
  await user.type(slug, "x");
  let event = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(true);
  await user.keyboard("{Backspace}");
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  event = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
  await user.type(slug, "-pending");
  await act(async () => { await s.client.entities.update("g1", { cover: { auto: true } }); });
  await waitFor(() => expect(s.calls.filter((call) => call === "GET /manage/entities/g1").length).toBeGreaterThan(1));
  expect(slug).toHaveValue("studio-pending");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
});

it("EntityManager confirms discarding edits before Back or changing kind", async () => {
  const s = editableServer();
  const user = userEvent.setup();
  render(<EntityManager entityKinds={["channel", "tag"]} />, { wrapper: wrap(s.client) });
  await user.click(await screen.findByRole("button", { name: "Studio" }));
  const slug = await screen.findByRole("textbox", { name: "Slug" });
  await user.type(slug, "-pending");
  await user.click(screen.getByRole("button", { name: "Back to the list" }));
  expect(await screen.findByRole("alertdialog")).toHaveTextContent("Leave this entity?");
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  expect(slug).toHaveValue("studio-pending");
  await user.click(screen.getByRole("tab", { name: "Tag" }));
  expect(await screen.findByRole("alertdialog")).toHaveTextContent("Discard unsaved changes");
  await user.click(screen.getByRole("button", { name: "Discard changes" }));
  await screen.findByRole("heading", { name: "Tag" });
  expect(screen.queryByRole("textbox", { name: "Slug" })).toBeNull();
});

it("names a new entity's kind in the singular and says what Create still needs", async () => {
  const s = server({ "GET /kinds": () => model });
  const user = userEvent.setup();
  render(<EntityEditor id={null} kind="channel" management labels={{ channel: { one: "Channel", other: "Channels" } }} />, { wrapper: wrap(s.client) });
  expect(await screen.findByRole("heading", { name: "New Channel" })).toBeVisible();
  const create = screen.getByRole("button", { name: "Create" });
  expect(create).toBeDisabled();
  expect(create).toHaveAccessibleDescription("Still needed: Name (English), Slug");
  await user.type(screen.getByRole("textbox", { name: /Name \(English\)/ }), "Studio");
  expect(screen.getByRole("textbox", { name: /Slug/ })).toHaveValue("studio");
  expect(create).toBeEnabled();
  expect(screen.queryByText(/Still needed/)).toBeNull();
});

// @vitest-environment jsdom
import "../../test/dom.js";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentFilterSettings } from "./filters.js";
import { useContentFilters, useContents } from "../../react/discovery.js";

const inventory = (filters: unknown[]) => ({ applies_to: ["gallery", "blog"], filters, include_deleted: false });
afterEach(() => { cleanup(); localStorage.clear(); });

it("renders nothing after loading when the site has no configurable filters", async () => {
  const fetcher = vi.fn(async () => Response.json(inventory([])));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  const { container } = render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  await waitFor(() => expect(container).toBeEmptyDOMElement());
});

it("retains the separate deleted-content option without an empty-filter message", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json(inventory([])) });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings /></ContentKitProvider>);
  await screen.findByRole("switch");
  expect(screen.queryByText("This site has no content filters.")).toBeNull();
});

it("uses localized entity names and patches only the selected filter", async () => {
  const filter = { entity: { id: "filter-group", slug: "explicit", name: "Explicit content" }, label: "Obsolete override", default_visible: false, visible: false, overridden: false };
  const other = { ...filter, entity: { id: "other", slug: "other", name: "Other content" } };
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => Response.json({ ...inventory([init?.method === "PATCH" ? { ...filter, visible: true, overridden: true } : filter, other]), include_deleted: true }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Explicit content" });
  expect(toggle).not.toBeChecked();
  expect(screen.queryByText("Hidden")).toBeNull();
  await userEvent.setup().click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  expect(screen.getByRole("switch", { name: "Other content" })).not.toBeChecked();
  expect(screen.queryByText("Shown")).toBeNull();
  expect(screen.queryByRole("button", { name: "Use the default" })).toBeNull();
  const update = fetcher.mock.calls.find(([, init]) => init?.method === "PATCH");
  expect(JSON.parse(String(update?.[1]?.body))).toEqual({ filters: [{ entity_id: "filter-group", visible: true }] });
});

it("lets guests save shared filters locally and responds to another tab without authenticated writes", async () => {
  const filter = { entity: { id: "guest-filter", slug: "explicit", name: "Explicit content" }, default_visible: false, visible: false, overridden: false };
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes("content-filters")) return Response.json(inventory([filter]));
    const visible = new Headers(init?.headers).get("ContentKit-Reader-Preferences")?.includes('"visible":true');
    return Response.json({ contents: visible ? [{ id: "visible", title: "Visible item" }] : [] });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/guest-filters", fetch: fetcher });
  function Feed() {
    const list = useContents({ kind: "gallery" });
    return <div>{list.items.map((item) => <span key={item.id}>{item.title}</span>)}</div>;
  }
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings /><Feed /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Explicit content" });
  expect(screen.queryByText("Include deleted content I own")).toBeNull();
  await userEvent.setup().click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  expect(await screen.findByText("Visible item")).toBeVisible();
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "guest-filter", visible: true }]);
  expect(fetcher.mock.calls.every((args) => (args[1] as RequestInit | undefined)?.method === "GET")).toBe(true);
  const key = Object.keys(localStorage).find((name) => name.includes("guest-filters"))!;
  expect(key).toContain("reader-preferences:v2:");
  const next = JSON.stringify({ filters: [{ entity_id: "guest-filter", visible: false }] });
  localStorage.setItem(key, next);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: next }));
  await waitFor(() => expect(toggle).not.toBeChecked());
  await waitFor(() => expect(screen.queryByText("Visible item")).toBeNull());
});

it("retains guest choices for entities offered only on another domain", async () => {
  const filter = { entity: { id: "here", slug: "here", name: "Current domain" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/domain-preferences", fetch: async () => Response.json(inventory([filter])) });
  client.preferences.browser.setFilters([{ entity_id: "other-domain", visible: false }]);
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await userEvent.setup().click(await screen.findByRole("switch", { name: "Current domain" }));
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "here", visible: false }, { entity_id: "other-domain", visible: false }]);
});

it("offers one shared entity toggle for all applicable content kinds", async () => {
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared tag" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/shared-controls", fetch: async () => Response.json(inventory([filter])) });
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  const toggles = await screen.findAllByRole("switch", { name: "Shared tag" });
  expect(toggles).toHaveLength(1);
  expect(screen.queryByText("Galleries")).toBeNull();
  expect(screen.queryByText("Blog posts")).toBeNull();
  await userEvent.setup().click(toggles[0]!);
  expect(toggles[0]).not.toBeChecked();
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "shared", visible: false }]);
});

it("migrates a legacy choice into the shared control when one applicable kind is selected", async () => {
  const baseUrl = "https://content.test/shared-legacy";
  const key = `contentkit:reader-preferences:v1:${baseUrl}/v1`;
  const backup = JSON.stringify({ filters: [{ group_id: "shared", visible: false }] });
  localStorage.setItem(key, backup);
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared tag" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json(inventory([filter])) });
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings kinds={["gallery"]} includeDeleted={false} /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Shared tag" });
  expect(toggle).not.toBeChecked();
  expect(screen.getAllByRole("switch")).toHaveLength(1);
  await userEvent.setup().click(toggle);
  expect(toggle).toBeChecked();
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "shared", visible: true }]);
  expect(localStorage.getItem(key)).toBe(backup);
});

it("resets one entity without resurrecting its legacy choice or changing other entities", async () => {
  const baseUrl = "https://content.test/shared-reset";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "shared", visible: false }, { group_id: "other", visible: false }] }));
  const filters = ["shared", "other"].map((id) => ({ entity: { id, slug: id, name: id }, default_visible: true, visible: true, overridden: false }));
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json(inventory(filters)) });
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.filters[0]?.visible).toBe(false));
  await act(async () => { await result.current.save({ filters: [{ entity_id: "shared", default: true }] }); });
  expect(result.current.filters?.filters).toMatchObject([{ entity: { id: "shared" }, visible: true, overridden: false }, { entity: { id: "other" }, visible: false, overridden: true }]);
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "other", visible: false }, { entity_id: "shared", default: true }]);
});

it("follows a new domain default after resetting the shared entity", async () => {
  const baseUrl = "https://content.test/domain-default-reset";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "shared", visible: false }] }));
  let defaultVisible = true;
  const entity = { id: "shared", slug: "shared", name: "Shared" };
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json(inventory([{ entity, default_visible: defaultVisible, visible: defaultVisible, overridden: false }])) });
  const { result, rerender } = renderHook(({ language }) => useContentFilters({ language }), { initialProps: { language: "en" }, wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.filters[0]?.visible).toBe(false));
  await act(async () => { await result.current.save({ filters: [{ entity_id: "shared", default: true }] }); });
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "shared", default: true }]);
  expect(result.current.filters?.filters[0]).toMatchObject({ visible: true, overridden: false });
  defaultVisible = false;
  rerender({ language: "es" });
  await waitFor(() => expect(result.current.filters?.filters[0]).toMatchObject({ visible: false, overridden: false }));
});

it("changes one of seventy migrated choices while retaining the other sixty-nine", async () => {
  const baseUrl = "https://content.test/many-legacy-choices";
  const legacy = Array.from({ length: 70 }, (_, i) => ({ group_id: `tag-${i}`, visible: false }));
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: legacy }));
  const rows = legacy.map((row) => ({ entity: { id: row.group_id, name: row.group_id, slug: row.group_id }, default_visible: true, visible: true, overridden: false }));
  const headers: string[] = [];
  const client = createContentKitClient({ baseUrl, fetch: async (url, init) => {
    const header = new Headers(init?.headers).get("ContentKit-Reader-Preferences");
    if (header) headers.push(header);
    return Response.json(String(url).includes("content-filters") ? inventory(rows) : { contents: [] });
  } });
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.filters).toHaveLength(70));
  await act(async () => { await result.current.setVisible("tag-0", true); });
  const saved = client.preferences.browser.snapshot().filters;
  expect(saved).toHaveLength(70);
  expect(saved.find((row) => row.entity_id === "tag-0")).toEqual({ entity_id: "tag-0", visible: true });
  expect(saved.filter((row) => row.visible === false)).toHaveLength(69);
  await expect(client.discovery.list()).resolves.toEqual({ contents: [] });
  expect(JSON.parse(headers.at(-1)!)).toEqual({ filters: saved });
  expect(new TextEncoder().encode(headers.at(-1)!).length).toBeLessThan(8192);
});

it("resets one choice in a large inventory without submitting every offered entity", async () => {
  const rows = Array.from({ length: 129 }, (_, i) => ({ entity: { id: `tag-${i}`, name: `Tag ${i}`, slug: `tag-${i}` }, default_visible: true, visible: true, overridden: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/reset-large-inventory", fetch: async () => Response.json(inventory(rows)) });
  client.preferences.browser.setFilters([{ entity_id: "tag-0", visible: false }]);
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.filters).toHaveLength(129));
  await act(async () => { await result.current.save({ filters: [{ entity_id: "tag-0", default: true }] }); });
  expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "tag-0", default: true }]);
  expect(result.current.filters?.filters.every((row) => row.visible && !row.overridden)).toBe(true);
});

it("rejects an invalid or duplicate guest patch atomically without losing existing choices", async () => {
  const row = { entity: { id: "known", name: "Known", slug: "known" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/atomic-guest-filter", fetch: async () => Response.json(inventory([row])) });
  client.preferences.browser.setFilters([{ entity_id: "off-domain", visible: false }]);
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.filters).toHaveLength(1));
  const before = client.preferences.browser.snapshot();
  await expect(result.current.save({ filters: [{ entity_id: "known", visible: false }, { entity_id: "unknown", visible: true }] })).rejects.toMatchObject({ code: "invalid_request" });
  expect(client.preferences.browser.snapshot()).toEqual(before);
  await expect(result.current.save({ filters: [{ entity_id: "known", visible: false }, { entity_id: "known", visible: true }] })).rejects.toMatchObject({ code: "invalid_request" });
  expect(client.preferences.browser.snapshot()).toEqual(before);
});

it("keeps independent account edits when their full responses arrive out of order", async () => {
  const snapshot = (a: boolean, b: boolean) => inventory([
    { entity: { id: "entity-a", name: "A", slug: "a" }, default_visible: false, visible: a, overridden: a },
    { entity: { id: "entity-b", name: "B", slug: "b" }, default_visible: false, visible: b, overridden: b },
  ]);
  let releaseA!: () => void;
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method !== "PATCH") return Response.json(snapshot(false, false));
    const update = JSON.parse(String(init.body));
    if (update.filters[0].entity_id === "entity-a") {
      const captured = Response.json(snapshot(true, false));
      return new Promise<Response>((resolve) => { releaseA = () => resolve(captured); });
    }
    return Response.json(snapshot(true, true));
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/account-filter-response-order", fetch: fetcher });
  const { result } = renderHook(() => ({ one: useContentFilters(), two: useContentFilters() }), {
    wrapper: ({ children }) => <ContentKitProvider client={client} viewer="reader">{children}</ContentKitProvider>,
  });
  await waitFor(() => expect(result.current.one.filters?.filters).toHaveLength(2));
  let pendingA!: Promise<unknown>;
  act(() => { pendingA = result.current.one.setVisible("entity-a", true); });
  await waitFor(() => expect(releaseA).toBeTypeOf("function"));
  await act(async () => { await result.current.two.setVisible("entity-b", true); });
  expect(result.current.two.filters?.filters.find((row) => row.entity.id === "entity-b")?.visible).toBe(true);
  await act(async () => { releaseA(); await pendingA; });
  for (const hook of [result.current.one, result.current.two]) {
    expect(hook.filters?.filters.map((row) => [row.entity.id, row.visible])).toEqual([["entity-a", true], ["entity-b", true]]);
  }
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === "PATCH")).toHaveLength(2);
});

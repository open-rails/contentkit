// @vitest-environment jsdom
import "../../test/dom.js";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentFilterSettings } from "./filters.js";
import { useContentFilters, useContents } from "../../react/discovery.js";

afterEach(() => { cleanup(); localStorage.clear(); });

it("renders nothing after loading when the site has no configurable filters", async () => {
  const fetcher = vi.fn(async () => Response.json({ kinds: { gallery: [] }, include_deleted: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  const { container } = render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  await waitFor(() => expect(container).toBeEmptyDOMElement());
});

it("retains the separate deleted-content option without an empty-filter message", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ kinds: { gallery: [] }, include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings /></ContentKitProvider>);
  await screen.findByRole("switch");
  expect(screen.queryByText("This site has no content filters.")).toBeNull();
});

it("uses localized entity names and saves the domain’s offered filters", async () => {
  const filter = { entity: { id: "filter-group", slug: "explicit", name: "Explicit content" }, label: "Obsolete override", visible: false, overridden: false };
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PUT") return Response.json({ kinds: { gallery: [{ ...filter, visible: true, overridden: true }] }, include_deleted: false });
    return Response.json({ kinds: { gallery: [filter] }, include_deleted: false });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Explicit content" });
  expect(toggle).not.toBeChecked();
  expect(screen.queryByText("Hidden")).toBeNull();
  await userEvent.setup().click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  expect(screen.queryByText("Shown")).toBeNull();
  expect(screen.queryByRole("button", { name: "Use the default" })).toBeNull();
  const update = fetcher.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(JSON.parse(String(update?.[1]?.body))).toEqual({ kinds: { gallery: [{ entity_id: "filter-group", visible: true }] }, include_deleted: false });
});

it("lets guests save filters locally and responds to another tab without authenticated writes", async () => {
  const filter = { entity: { id: "guest-filter", slug: "explicit", name: "Explicit content" }, label: "Explicit content", default_visible: false, visible: false, overridden: false };
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes("content-filters")) return Response.json({ kinds: { gallery: [filter] }, include_deleted: false });
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
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([{ entity_id: "guest-filter", visible: true }]);
  expect(fetcher.mock.calls.every((args) => (args[1] as RequestInit | undefined)?.method === "GET")).toBe(true);
  const key = Object.keys(localStorage).find((name) => name.includes("guest-filters"))!;
  const next = JSON.stringify({ kinds: { gallery: [{ entity_id: "guest-filter", visible: false }] } });
  localStorage.setItem(key, next);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: next }));
  await waitFor(() => expect(toggle).not.toBeChecked());
  await waitFor(() => expect(screen.queryByText("Visible item")).toBeNull());
});


it("retains guest choices for entities offered only on another domain", async () => {
  const filter = { entity: { id: "here", slug: "here", name: "Current domain" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/domain-preferences", fetch: async () => Response.json({ kinds: { gallery: [filter] }, include_deleted: false }) });
  client.preferences.browser.setFilters({ gallery: [{ entity_id: "other-domain", visible: false }] });
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await userEvent.setup().click(await screen.findByRole("switch", { name: "Current domain" }));
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([
    { entity_id: "here", visible: false },
    { entity_id: "other-domain", visible: false },
  ]);
});

it("keeps the same entity independent between content kinds", async () => {
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared tag" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/kind-controls", fetch: async () => Response.json({ kinds: { gallery: [filter], blog: [filter] }, include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings includeDeleted={false} labels={{ gallery: "Galleries", blog: "Blog posts" }} /></ContentKitProvider>);
  const toggles = await screen.findAllByRole("switch", { name: "Shared tag" });
  expect(screen.getByText("Galleries")).toBeVisible();
  expect(screen.getByText("Blog posts")).toBeVisible();
  await userEvent.setup().click(toggles[0]!);
  expect(toggles[0]).not.toBeChecked();
  expect(toggles[1]).toBeChecked();
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([{ entity_id: "shared", visible: false }]);
  expect(client.preferences.browser.snapshot().kinds.blog ?? []).toEqual([]);
});

it("retains legacy choices while allowing a kind-specific override and selected kind controls", async () => {
  const baseUrl = "https://content.test/kind-legacy";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "shared", visible: false }] }));
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared tag" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json({ kinds: { gallery: [filter], blog: [filter] }, include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings kinds={["gallery"]} includeDeleted={false} /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Shared tag" });
  expect(toggle).not.toBeChecked();
  expect(screen.getAllByRole("switch")).toHaveLength(1);
  expect(screen.queryByText("Gallery")).toBeNull();
  await userEvent.setup().click(toggle);
  expect(toggle).toBeChecked();
  expect(client.preferences.browser.snapshot().legacy_filters).toEqual([{ entity_id: "shared", visible: false }]);
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([{ entity_id: "shared", visible: true }]);
  expect(client.preferences.browser.snapshot().kinds.blog).toBeUndefined();
});


it("resets one kind without resurrecting legacy choices or changing other kinds", async () => {
  const baseUrl = "https://content.test/kind-reset";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "shared", visible: false }] }));
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json({ kinds: { gallery: [filter], blog: [filter] }, include_deleted: false }) });
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.kinds.gallery?.[0]?.visible).toBe(false));
  await act(async () => { await result.current.save({ kinds: { gallery: [] }, include_deleted: false }); });
  expect(result.current.filters?.kinds.gallery?.[0]?.visible).toBe(true);
  expect(result.current.filters?.kinds.blog?.[0]?.visible).toBe(false);
  expect(client.preferences.browser.snapshot().legacy_filters).toEqual([{ entity_id: "shared", visible: false }]);
});

it("follows a new domain default after resetting only that kind", async () => {
  const baseUrl = "https://content.test/domain-default-reset";
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: [{ group_id: "shared", visible: false }] }));
  let galleryDefault = true;
  const filter = { entity: { id: "shared", slug: "shared", name: "Shared" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl, fetch: async () => Response.json({ kinds: { gallery: [{ ...filter, default_visible: galleryDefault }], blog: [filter] }, include_deleted: false }) });
  const { result, rerender } = renderHook(({ language }) => useContentFilters({ language }), { initialProps: { language: "en" }, wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.kinds.gallery?.[0]?.visible).toBe(false));
  await act(async () => { await result.current.save({ kinds: { gallery: [] }, include_deleted: false }); });
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([{ entity_id: "shared", default: true }]);
  expect(result.current.filters?.kinds.gallery?.[0]).toMatchObject({ visible: true, overridden: false });
  galleryDefault = false;
  rerender({ language: "es" });
  await waitFor(() => expect(result.current.filters?.kinds.gallery?.[0]).toMatchObject({ visible: false, overridden: false }));
  expect(result.current.filters?.kinds.blog?.[0]).toMatchObject({ visible: false, overridden: true });
});

it("changes one of seventy legacy choices without promoting the other sixty-nine", async () => {
  const baseUrl = "https://content.test/many-legacy-choices";
  const legacy = Array.from({ length: 70 }, (_, i) => ({ group_id: `tag-${i}`, visible: false }));
  localStorage.setItem(`contentkit:reader-preferences:v1:${baseUrl}/v1`, JSON.stringify({ filters: legacy }));
  const rows = legacy.map((row) => ({ entity: { id: row.group_id, name: row.group_id, slug: row.group_id }, default_visible: true, visible: true, overridden: false }));
  const headers: string[] = [];
  const client = createContentKitClient({ baseUrl, fetch: async (url, init) => {
    const header = new Headers(init?.headers).get("ContentKit-Reader-Preferences");
    if (header) headers.push(header);
    return Response.json(String(url).includes("content-filters") ? { kinds: { gallery: rows }, include_deleted: false } : { contents: [] });
  } });
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.kinds.gallery).toHaveLength(70));
  await act(async () => { await result.current.setVisible("gallery", "tag-0", true); });
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([{ entity_id: "tag-0", visible: true }]);
  expect(client.preferences.browser.snapshot().legacy_filters).toHaveLength(70);
  await expect(client.discovery.list()).resolves.toEqual({ contents: [] });
  const sent = JSON.parse(headers.at(-1)!);
  expect(sent.kinds.gallery).toHaveLength(1);
  expect(sent.legacy_filters).toHaveLength(69);
  expect(new TextEncoder().encode(headers.at(-1)!).length).toBeLessThan(8192);
});

it("resets a large explicit-only inventory without adding needless default markers", async () => {
  const rows = Array.from({ length: 129 }, (_, i) => ({ entity: { id: `tag-${i}`, name: `Tag ${i}`, slug: `tag-${i}` }, default_visible: true, visible: true, overridden: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/reset-large-inventory", fetch: async () => Response.json({ kinds: { gallery: rows }, include_deleted: false }) });
  client.preferences.browser.setFilters({ gallery: [{ entity_id: "tag-0", visible: false }] });
  const { result } = renderHook(() => useContentFilters(), { wrapper: ({ children }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider> });
  await waitFor(() => expect(result.current.filters?.kinds.gallery).toHaveLength(129));
  await act(async () => { await result.current.save({ kinds: { gallery: [] }, include_deleted: false }); });
  expect(client.preferences.browser.snapshot().kinds.gallery).toEqual([]);
  expect(result.current.filters?.kinds.gallery?.every((row) => row.visible && !row.overridden)).toBe(true);
});

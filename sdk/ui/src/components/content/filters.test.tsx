// @vitest-environment jsdom
import "../../test/dom.js";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentFilterSettings } from "./filters.js";
import { useContents } from "../../react/discovery.js";

afterEach(() => { cleanup(); localStorage.clear(); });

it("renders nothing after loading when the site has no configurable filters", async () => {
  const fetcher = vi.fn(async () => Response.json({ filters: [], include_deleted: false }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  const { container } = render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  await waitFor(() => expect(container).toBeEmptyDOMElement());
});

it("retains the separate deleted-content option without an empty-filter message", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ filters: [], include_deleted: false }) });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings /></ContentKitProvider>);
  await screen.findByRole("switch");
  expect(screen.queryByText("This site has no content filters.")).toBeNull();
});

it("uses localized entity names and saves the domain’s offered filters", async () => {
  const filter = { entity: { id: "filter-group", slug: "explicit", name: "Explicit content" }, label: "Obsolete override", visible: false, overridden: false };
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PUT") return Response.json({ filters: [{ ...filter, visible: true, overridden: true }], include_deleted: false });
    return Response.json({ filters: [filter], include_deleted: false });
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
  expect(JSON.parse(String(update?.[1]?.body))).toEqual({ filters: [{ group_id: "filter-group", visible: true }], include_deleted: false });
});

it("lets guests save filters locally and responds to another tab without authenticated writes", async () => {
  const filter = { entity: { id: "guest-filter", slug: "explicit", name: "Explicit content" }, label: "Explicit content", default_visible: false, visible: false, overridden: false };
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes("content-filters")) return Response.json({ filters: [filter], include_deleted: false });
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
  expect(client.preferences.browser.snapshot().filters).toEqual([{ group_id: "guest-filter", visible: true }]);
  expect(fetcher.mock.calls.every((args) => (args[1] as RequestInit | undefined)?.method === "GET")).toBe(true);
  const key = Object.keys(localStorage).find((name) => name.includes("guest-filters"))!;
  const next = JSON.stringify({ filters: [{ group_id: "guest-filter", visible: false }] });
  localStorage.setItem(key, next);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: next }));
  await waitFor(() => expect(toggle).not.toBeChecked());
  await waitFor(() => expect(screen.queryByText("Visible item")).toBeNull());
});


it("retains guest choices for entities offered only on another domain", async () => {
  const filter = { entity: { id: "here", slug: "here", name: "Current domain" }, default_visible: true, visible: true, overridden: false };
  const client = createContentKitClient({ baseUrl: "https://content.test/domain-preferences", fetch: async () => Response.json({ filters: [filter], include_deleted: false }) });
  client.preferences.browser.setFilters([{ group_id: "other-domain", visible: false }]);
  render(<ContentKitProvider client={client} viewer={null}><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  await userEvent.setup().click(await screen.findByRole("switch", { name: "Current domain" }));
  expect(client.preferences.browser.snapshot().filters).toEqual([
    { group_id: "here", visible: false },
    { group_id: "other-domain", visible: false },
  ]);
});

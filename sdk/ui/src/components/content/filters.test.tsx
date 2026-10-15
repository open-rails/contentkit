// @vitest-environment jsdom
import "../../test/dom.js";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentFilterSettings } from "./filters.js";

afterEach(cleanup);

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

it("keeps configured filters visible and saves reader changes", async () => {
  const filter = { group: { id: "filter-group", slug: "explicit", name: "Explicit content" }, label: "Explicit content", visible: false, overridden: false };
  const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PUT") return Response.json({ filters: [{ ...filter, visible: true, overridden: true }], include_deleted: false });
    return Response.json({ filters: [filter], include_deleted: false });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="reader"><ContentFilterSettings includeDeleted={false} /></ContentKitProvider>);
  const toggle = await screen.findByRole("switch", { name: "Explicit content" });
  expect(toggle).not.toBeChecked();
  await userEvent.setup().click(toggle);
  await waitFor(() => expect(toggle).toBeChecked());
  const update = fetcher.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(JSON.parse(String(update?.[1]?.body))).toEqual({ filters: [{ group_id: "filter-group", visible: true }], include_deleted: false });
});

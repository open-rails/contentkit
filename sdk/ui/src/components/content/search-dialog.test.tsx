// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { SearchDialog, type SearchDialogProps } from "./search-dialog.js";

function Search({ onSearch, ...props }: Partial<SearchDialogProps> & Pick<SearchDialogProps, "onSearch">) {
  const [open, setOpen] = useState(false);
  return <>
    <button onClick={() => setOpen(true)}>Open search</button>
    <SearchDialog {...props} open={open} onOpenChange={setOpen} onSearch={onSearch} />
  </>;
}

it("focuses search, offers empty history, and returns focus when dismissed", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/dialog-focus", fetch: async () => Response.json({ contents: [], entities: [] }) });
  const onSearch = vi.fn();
  render(<ContentKitProvider client={client} viewer={null}><Search onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const opener = screen.getByRole("button", { name: "Open search" });
  expect(screen.queryByRole("dialog")).toBeNull();
  await user.click(opener);
  const dialog = screen.getByRole("dialog", { name: "Search" });
  expect(await within(dialog).findByText("No recent searches yet.")).toBeVisible();
  expect(within(dialog).getByRole("combobox", { name: "Search" })).toHaveFocus();
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(opener).toHaveFocus();
  await user.click(opener);
  await user.click(screen.getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(opener).toHaveFocus();
  expect(onSearch).not.toHaveBeenCalled();
});

it("submits a trimmed query and closes exactly once after selection", async () => {
  const onSearch = vi.fn(), onSelect = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/dialog-submit", fetch: async () => Response.json({ contents: [], entities: [] }) });
  render(<ContentKitProvider client={client} viewer={null}><Search onSearch={onSearch} onSelect={onSelect} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Open search" }));
  await user.type(screen.getByRole("combobox", { name: "Search" }), "  moon  ");
  await user.keyboard("{Enter}");
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("moon");
  expect(onSelect).toHaveBeenCalledExactlyOnceWith();
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("closes on suggestion navigation without submitting a results search", async () => {
  const navigate = vi.fn(), onSearch = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/dialog-suggestion", fetch: async () => Response.json({ contents: [], entities: [{ id: "series", kind: "series", name: "Moon series", slug: "moon", path: "/series/moon" }] }) });
  render(<ContentKitProvider client={client} viewer={null} navigate={navigate}><Search onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Open search" }));
  await user.type(screen.getByRole("combobox", { name: "Search" }), "moon");
  await screen.findByRole("option", { name: /Moon series/ });
  await user.keyboard("{ArrowDown}{Enter}");
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/series/moon");
  expect(onSearch).not.toHaveBeenCalled();
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("keeps history removal inside the dialog without selecting a search", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/dialog-history", fetch: async () => Response.json({ contents: [], entities: [] }) });
  client.discovery.browserSearches.record("Moon history", 1);
  const onSearch = vi.fn();
  render(<ContentKitProvider client={client} viewer={null}><Search onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Open search" }));
  await user.click(await screen.findByRole("button", { name: "Remove “Moon history”" }));
  expect(await screen.findByText("No recent searches yet.")).toBeVisible();
  expect(screen.getByRole("dialog", { name: "Search" })).toBeVisible();
  expect(onSearch).not.toHaveBeenCalled();
});

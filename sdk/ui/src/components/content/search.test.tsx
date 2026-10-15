// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { SearchBox } from "./search.js";

it("opens an empty recent-search panel on focus, reopens on click, and closes outside or on Escape", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async () => Response.json({ searches: [] }) });
  render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={() => {}} /><button>Elsewhere</button></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  expect(input).toBeVisible();
  expect(input).toHaveAttribute("aria-expanded", "false");
  await user.tab();
  expect(input).toHaveAttribute("aria-expanded", "true");
  expect(await screen.findByText("No recent searches yet.")).toBeVisible();
  expect(screen.getByRole("listbox")).toBeVisible();
  expect(screen.queryByRole("dialog")).toBeNull();
  await user.keyboard("{Escape}");
  expect(input).toHaveFocus();
  expect(input).toHaveAttribute("aria-expanded", "false");
  await user.click(input);
  expect(input).toHaveAttribute("aria-expanded", "true");
  await user.click(screen.getByRole("button", { name: "Elsewhere" }));
  expect(input).toHaveAttribute("aria-expanded", "false");
});

it("shows loading and empty-result states while preserving keyboard submit and suggestion navigation", async () => {
  let complete: ((value: Response) => void) | undefined;
  const onSearch = vi.fn(), navigate = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: vi.fn(async (url) => {
    if (String(url).includes("/typeahead")) return new Promise<Response>((resolve) => { complete = resolve; });
    return Response.json({ searches: [] });
  }) });
  render(<ContentKitProvider client={client} viewer={null} navigate={navigate}><SearchBox onSearch={onSearch} href={(item) => `/custom/${item.id}`} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.type(input, "cat");
  expect(screen.getByRole("status")).toHaveTextContent("Loading…");
  await waitFor(() => expect(complete).toBeTypeOf("function"));
  complete!(Response.json({ contents: [], groups: [] }));
  expect(await screen.findByText("Nothing matches “cat”.")).toBeVisible();
  await user.keyboard("{Enter}");
  expect(onSearch).toHaveBeenLastCalledWith("cat");
  await user.click(input);
  complete = undefined;
  await user.type(input, "s");
  await waitFor(() => expect(complete).toBeTypeOf("function"));
  complete!(Response.json({ contents: [], groups: [{ id: "artist", kind: "artist", name: "Cats", slug: "cats", path: "/artists/cats" }] }));
  const option = await screen.findByRole("option", { name: /Cats/ });
  await user.keyboard("{ArrowDown}");
  expect(option).toHaveAttribute("aria-selected", "true");
  await user.keyboard("{Enter}");
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/custom/artist");
  expect(input).toHaveAttribute("aria-expanded", "false");
});

it("uses recent searches and permits clearing them without submitting a search", async () => {
  let searches = [{ query: "old query" }];
  const onSearch = vi.fn();
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "DELETE") { searches = []; return new Response(null, { status: 204 }); }
    return Response.json({ searches });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.click(input);
  await screen.findByRole("option", { name: /old query/ });
  await user.click(screen.getByRole("button", { name: "Remove “old query”" }));
  expect(await screen.findByText("No recent searches yet.")).toBeVisible();
  expect(onSearch).not.toHaveBeenCalled();
  expect(fetcher.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(true);
});

it("submits a recent query from the keyboard and keeps typed text when closing", async () => {
  const onSearch = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url) => String(url).includes("/typeahead") ? Response.json({ contents: [], groups: [] }) : Response.json({ searches: [{ query: "old query" }] }) });
  render(<ContentKitProvider client={client}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.click(input);
  await screen.findByRole("option", { name: /old query/ });
  await user.keyboard("{ArrowDown}{Enter}");
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("old query");
  expect(input).toHaveValue("old query");
  await user.click(input);
  await user.keyboard("{Escape}");
  expect(input).toHaveValue("old query");
  expect(input).toHaveAttribute("aria-expanded", "false");
});

it("keeps public search usable with local guest recents and no account history request", async () => {
  const onSearch = vi.fn();
  const fetcher = vi.fn(async (url: RequestInfo | URL) => String(url).includes("/typeahead") ? Response.json({ contents: [], groups: [] }) : Response.json({ error: { code: "authentication_required", message: "Sign in" } }, { status: 401 }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.click(input);
  expect(await screen.findByText("No recent searches yet.")).toBeVisible();
  expect(screen.queryByRole("alert")).toBeNull();
  await user.type(input, "public{Enter}");
  expect(onSearch).toHaveBeenCalledWith("public");
  expect(fetcher.mock.calls.every(([url]) => !String(url).includes("/me/searches"))).toBe(true);
});

it("refreshes account recents when the actual search panel reopens", async () => {
  let query = "first account query";
  const onSearch = vi.fn();
  const fetcher = vi.fn(async () => Response.json({ searches: [{ query, results: 1, searched_at: "2026-10-15T12:00:00Z" }] }));
  const client = createContentKitClient({ baseUrl: "https://content.test/panel-reopen", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="alice"><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  expect(fetcher).not.toHaveBeenCalled();
  await user.click(input);
  await screen.findByRole("option", { name: /first account query/ });
  await user.keyboard("{Escape}");
  expect(input).toHaveAttribute("aria-expanded", "false");
  query = "new account query";
  await user.keyboard("{ArrowDown}");
  const option = await screen.findByRole("option", { name: /new account query/ });
  expect(option).toHaveAttribute("aria-selected", "true");
  expect(screen.queryByRole("option", { name: /first account query/ })).toBeNull();
  expect(fetcher).toHaveBeenCalledTimes(2);
  await user.keyboard("{Enter}");
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("new account query");
  expect(input).toHaveAttribute("aria-expanded", "false");
});

it("anchors the full-width header presentation below the header and follows resize and scroll", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/header-geometry", fetch: async () => Response.json({ searches: [] }) });
  const { container } = render(<ContentKitProvider client={client} viewer={null}><header><SearchBox presentation="header" appearance={{ theme: "dark" }} onSearch={() => {}} /></header></ContentKitProvider>);
  const header = container.querySelector("header")!;
  let bottom = 84;
  vi.spyOn(header, "getBoundingClientRect").mockImplementation(() => ({ bottom } as DOMRect));
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  const layer = document.querySelector('[data-ckui="search-layer"]')!;
  const panel = document.querySelector<HTMLDivElement>('[data-ckui="search-panel"]')!;
  expect(header.contains(panel)).toBe(false);
  expect(layer).toHaveStyle({ top: "84px" });
  expect(layer).toHaveAttribute("data-ckui-theme", "dark");
  expect(layer).toHaveClass("fixed", "inset-x-0");
  expect(document.querySelector('[data-ckui="search-backdrop"]')).toHaveClass("bg-black/30", "backdrop-blur-sm");
  expect(panel.querySelector(".max-w-\\[71rem\\]")).not.toBeNull();
  expect(screen.queryByRole("dialog")).toBeNull();
  bottom = 116;
  fireEvent.resize(window);
  expect(layer).toHaveStyle({ top: "116px" });
  bottom = 62;
  fireEvent.scroll(window);
  expect(layer).toHaveStyle({ top: "62px" });
  await user.click(document.querySelector('[data-ckui="search-backdrop"]')!);
  expect(document.querySelector('[data-ckui="search-layer"]')).toBeNull();
});

it("uses the input as a header fallback and offers the archived clear/close control", async () => {
  const onSearch = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/header-input", fetch: async () => Response.json({ contents: [], groups: [] }) });
  render(<ContentKitProvider client={client} viewer={null}><SearchBox presentation="header" onSearch={onSearch} /></ContentKitProvider>);
  const input = screen.getByRole("combobox", { name: "Search" });
  vi.spyOn(input, "getBoundingClientRect").mockReturnValue({ bottom: 48 } as DOMRect);
  const user = userEvent.setup();
  await user.type(input, "cats");
  expect(document.querySelector('[data-ckui="search-layer"]')).toHaveStyle({ top: "48px" });
  expect(input).toHaveClass("rounded-full");
  await user.click(screen.getByRole("button", { name: "Search" }));
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("cats");
  await user.click(input);
  await user.click(screen.getByRole("button", { name: "Clear search" }));
  expect(input).toHaveValue("");
  expect(input).toHaveAttribute("aria-expanded", "true");
  await user.click(screen.getByRole("button", { name: "Close" }));
  expect(input).toHaveAttribute("aria-expanded", "false");
});

it("renders one flat header list with colored type icons, provider order, and clickable portal results", async () => {
  const navigate = vi.fn();
  const contents = [
    { id: "g", kind: "gallery", title: "Gallery result", path: "/gallery" },
    { id: "p", kind: "post", title: "Post result", path: "/post" },
  ];
  const groups = ["artist", "series", "tag", "character"].map((kind) => ({ id: kind, kind, name: `${kind} result`, path: `/${kind}` }));
  const client = createContentKitClient({ baseUrl: "https://content.test/header-results", fetch: async () => Response.json({ contents, groups }) });
  render(<ContentKitProvider client={client} viewer={null} navigate={navigate}><SearchBox presentation="header" onSearch={() => {}} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.type(screen.getByRole("combobox", { name: "Search" }), "result");
  await screen.findByRole("option", { name: /Gallery result/ });
  const panel = document.querySelector<HTMLDivElement>('[data-ckui="search-panel"]')!;
  expect(panel.querySelectorAll("img")).toHaveLength(0);
  expect([...panel.querySelectorAll("[data-search-kind]")].map((icon) => icon.getAttribute("data-search-kind"))).toEqual(["gallery", "post", "artist", "series", "tag", "character"]);
  expect(panel.querySelector('[data-search-kind="artist"]')).toHaveClass("text-blue-500");
  expect(panel.querySelector('[data-search-kind="series"]')).toHaveClass("text-yellow-600");
  expect(panel.querySelector('[data-search-kind="gallery"]')).toHaveClass("text-green-600");
  expect(panel.querySelector('[data-search-kind="tag"]')).toHaveClass("text-orange-500");
  await user.click(within(panel).getByRole("option", { name: /artist result/ }));
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/artist");
  expect(document.querySelector('[data-ckui="search-panel"]')).toBeNull();
});

it("keeps the default inline panel and content thumbnails", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/inline-cover", fetch: async () => Response.json({ contents: [{ id: "gallery", kind: "gallery", title: "Inline gallery", cover: [{ url: "https://media.test/cover.webp", w: 300, h: 400 }] }], groups: [] }) });
  const { container } = render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={() => {}} /></ContentKitProvider>);
  await userEvent.setup().type(screen.getByRole("combobox", { name: "Search" }), "gallery");
  await screen.findByRole("option", { name: "Inline gallery" });
  const panel = container.querySelector('[data-ckui="search-panel"]')!;
  expect(panel).not.toBeNull();
  expect(panel.querySelector("img")).toHaveAttribute("src", "https://media.test/cover.webp");
  expect(document.querySelector('[data-ckui="search-backdrop"]')).toBeNull();
});

it("shows recent dates and keeps portaled history controls working without navigating", async () => {
  let searches = [
    { query: "older", results: 1, searched_at: "2026-04-15T12:00:00Z" },
    { query: "newer", results: 2, searched_at: "2026-04-16T12:00:00Z" },
  ];
  const onSearch = vi.fn();
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "DELETE") {
      const query = new URL(String(url)).searchParams.get("query");
      searches = query ? searches.filter((s) => s.query !== query) : [];
      return new Response(null, { status: 204 });
    }
    return Response.json({ searches });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/header-recents", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="alice"><SearchBox presentation="header" onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.click(input);
  await screen.findByRole("option", { name: /older/ });
  expect(document.querySelector('time[datetime="2026-04-15T12:00:00Z"]')).toHaveTextContent(new Intl.DateTimeFormat(undefined, { dateStyle: "short" }).format(new Date("2026-04-15T12:00:00Z")));
  await user.click(screen.getByRole("button", { name: "Remove “older”" }));
  await waitFor(() => expect(screen.queryByRole("option", { name: /older/ })).toBeNull());
  expect(input).toHaveAttribute("aria-expanded", "true");
  const clear = screen.getByRole("button", { name: "Clear all" });
  clear.focus();
  await user.keyboard("{Escape}");
  expect(input).toHaveFocus();
  expect(input).toHaveAttribute("aria-expanded", "false");
  await user.click(input);
  const clearAgain = await screen.findByRole("button", { name: "Clear all" });
  clearAgain.focus();
  expect(input).toHaveAttribute("aria-expanded", "true");
  await user.keyboard("{Enter}");
  expect(await screen.findByText("Start typing to search.")).toBeVisible();
  expect(onSearch).not.toHaveBeenCalled();
});

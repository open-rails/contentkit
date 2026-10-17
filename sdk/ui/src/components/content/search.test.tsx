// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import { ContentKitProvider } from "../../react/provider.js";
import { SearchBox } from "./search.js";

it("shows matching history before entities and titles, ignoring fillers and favoring prefixes over interior matches", async () => {
  const onSearch = vi.fn(), navigate = vi.fn();
  let complete: ((response: Response) => void) | undefined;
  const fetcher = vi.fn(async (url: RequestInfo | URL) => {
    if (String(url).includes("/typeahead")) return new Promise<Response>((resolve) => { complete = resolve; });
    return Response.json({ searches: [{ query: "Discover Inazuma" }, { query: "Inazuma art" }, { query: "The Inazuma" }, { query: "INAZUMA manga" }, { query: "unrelated" }] });
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/prefix-history", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer="alice" navigate={navigate}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.type(input, "inazuma");
  await waitFor(() => expect(complete).toBeTypeOf("function"));
  expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Inazuma art", "The Inazuma", "INAZUMA manga", "Discover Inazuma"]);
  expect(fetcher.mock.calls.filter(([url]) => String(url).includes("/me/searches"))).toHaveLength(1);
  complete!(Response.json({ contents: [{ id: "gallery", kind: "gallery", title: "Inazuma Adventures", path: "/gallery" }], entities: [{ id: "artist", kind: "artist", name: "Inazuma", slug: "inazuma", path: "/artist" }] }));
  await screen.findByRole("option", { name: "Inazuma Adventures" });
  expect(screen.getAllByRole("option").map((o) => o.getAttribute("data-type"))).toEqual(["recent", "recent", "recent", "recent", "group", "content"]);
  await user.keyboard("{ArrowDown}{Enter}");
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("Inazuma art");
  expect(navigate).not.toHaveBeenCalled();
});

it("does not turn filler-only input into a wildcard over recent searches", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/filler-history", fetch: async () => Response.json({ contents: [], entities: [] }) });
  client.discovery.browserSearches.record("The Azure Journey", 1);
  const user = userEvent.setup();
  render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={vi.fn()} /></ContentKitProvider>);
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.type(input, "the a an");
  expect(screen.queryByRole("option")).not.toBeInTheDocument();
  await user.clear(input);
  await user.type(input, "THE journey");
  expect(screen.getByRole("option", { name: /The Azure Journey/ })).toBeVisible();
});

it("keeps matching guest recents usable without waiting for autocomplete and hides stale server suggestions", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/prefix-guest", fetch: async (url) => {
    const query = new URL(String(url)).searchParams.get("q");
    if (query === "light") return Response.json({ contents: [], entities: [{ id: "artist", kind: "artist", name: "Lightning", slug: "lightning", path: "/artist" }] });
    return new Promise<Response>(() => {});
  } });
  client.discovery.browserSearches.record("Café questions", 1);
  client.discovery.browserSearches.record("Lightning art", 1);
  const onSearch = vi.fn();
  render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.type(input, "light");
  await screen.findByRole("option", { name: /Lightning.*Artist/ });
  await user.clear(input);
  await user.type(input, " CAFE  ");
  expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Café questions"]);
  await user.keyboard("{ArrowDown}{Enter}");
  expect(onSearch).toHaveBeenCalledExactlyOnceWith("Café questions");
});

it("keeps the chosen entity selected when slower history arrives above it", async () => {
  let history: ((response: Response) => void) | undefined;
  const navigate = vi.fn(), onSearch = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/late-prefix-history", fetch: async (url) => {
    if (String(url).includes("/typeahead")) return Response.json({ contents: [], entities: [{ id: "artist", kind: "artist", name: "Inazuma", slug: "inazuma", path: "/artist" }] });
    return new Promise<Response>((resolve) => { history = resolve; });
  } });
  render(<ContentKitProvider client={client} viewer="alice" navigate={navigate}><SearchBox onSearch={onSearch} /></ContentKitProvider>);
  const user = userEvent.setup();
  const input = screen.getByRole("combobox", { name: "Search" });
  await user.type(input, "inaz");
  const artist = await screen.findByRole("option", { name: /Inazuma.*Artist/ });
  await user.keyboard("{ArrowDown}");
  expect(artist).toHaveAttribute("aria-selected", "true");
  history!(Response.json({ searches: [{ query: "Inazuma art" }] }));
  await screen.findByRole("option", { name: /Inazuma art/ });
  expect(artist).toHaveAttribute("aria-selected", "true");
  await user.keyboard("{Enter}");
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/artist");
  expect(onSearch).not.toHaveBeenCalled();
});

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
  complete!(Response.json({ contents: [], entities: [] }));
  expect(await screen.findByText("Nothing matches “cat”.")).toBeVisible();
  await user.keyboard("{Enter}");
  expect(onSearch).toHaveBeenLastCalledWith("cat");
  await user.click(input);
  complete = undefined;
  await user.type(input, "s");
  await waitFor(() => expect(complete).toBeTypeOf("function"));
  complete!(Response.json({ contents: [], entities: [{ id: "artist", kind: "artist", name: "Cats", slug: "cats", path: "/artists/cats" }] }));
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
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url) => String(url).includes("/typeahead") ? Response.json({ contents: [], entities: [] }) : Response.json({ searches: [{ query: "old query" }] }) });
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
  const fetcher = vi.fn(async (url: RequestInfo | URL) => String(url).includes("/typeahead") ? Response.json({ contents: [], entities: [] }) : Response.json({ error: { code: "authentication_required", message: "Sign in" } }, { status: 401 }));
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

it("anchors the header dropdown to the field width and follows resize and scroll", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/header-geometry", fetch: async () => Response.json({ searches: [] }) });
  const { container } = render(<ContentKitProvider client={client} viewer={null}><header><SearchBox presentation="header" appearance={{ theme: "dark" }} onSearch={() => {}} /></header></ContentKitProvider>);
  const header = container.querySelector("header")!;
  let bottom = 84, left = 120, width = 400;
  vi.spyOn(screen.getByRole("combobox", { name: "Search" }), "getBoundingClientRect").mockImplementation(() => ({ bottom, left, width } as DOMRect));
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  const layer = document.querySelector('[data-ckui="search-layer"]')!;
  const panel = document.querySelector<HTMLDivElement>('[data-ckui="search-panel"]')!;
  expect(header.contains(panel)).toBe(false);
  expect(layer).toHaveStyle({ top: "88px" });
  expect(panel).toHaveStyle({ marginLeft: "120px", width: "400px" });
  expect(layer).toHaveAttribute("data-ckui-theme", "dark");
  expect(layer).toHaveClass("fixed", "inset-x-0");
  expect(document.querySelector('[data-ckui="search-backdrop"]')).toHaveClass("bg-black/30");
  expect(screen.queryByRole("dialog")).toBeNull();
  bottom = 116;
  left = 160;
  width = 480;
  fireEvent.resize(window);
  expect(layer).toHaveStyle({ top: "120px" });
  expect(panel).toHaveStyle({ marginLeft: "160px", width: "480px" });
  bottom = 62;
  fireEvent.scroll(window);
  expect(layer).toHaveStyle({ top: "66px" });
  await user.click(document.querySelector('[data-ckui="search-backdrop"]')!);
  expect(document.querySelector('[data-ckui="search-layer"]')).toBeNull();
});

it("uses the input as a header fallback and offers the archived clear/close control", async () => {
  const onSearch = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/header-input", fetch: async () => Response.json({ contents: [], entities: [] }) });
  render(<ContentKitProvider client={client} viewer={null}><SearchBox presentation="header" onSearch={onSearch} /></ContentKitProvider>);
  const input = screen.getByRole("combobox", { name: "Search" });
  vi.spyOn(input, "getBoundingClientRect").mockReturnValue({ bottom: 48, left: 20, width: 320 } as DOMRect);
  const user = userEvent.setup();
  await user.type(input, "cats");
  expect(document.querySelector('[data-ckui="search-layer"]')).toHaveStyle({ top: "52px" });
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

it("renders one flat header list with entities before titles, colored type icons, and clickable portal results", async () => {
  const navigate = vi.fn();
  const contents = [
    { id: "g", kind: "gallery", title: "Gallery result", path: "/gallery" },
    { id: "p", kind: "post", title: "Post result", path: "/post" },
  ];
  const entities = ["artist", "series", "tag", "character"].map((kind) => ({ id: kind, kind, name: `${kind} result`, path: `/${kind}` }));
  const client = createContentKitClient({ baseUrl: "https://content.test/header-results", fetch: async () => Response.json({ contents, entities }) });
  render(<ContentKitProvider client={client} viewer={null} navigate={navigate}><SearchBox presentation="header" onSearch={() => {}} /></ContentKitProvider>);
  const user = userEvent.setup();
  await user.type(screen.getByRole("combobox", { name: "Search" }), "result");
  await screen.findByRole("option", { name: /Gallery result/ });
  const panel = document.querySelector<HTMLDivElement>('[data-ckui="search-panel"]')!;
  expect(panel.querySelectorAll("img")).toHaveLength(0);
  expect([...panel.querySelectorAll("[data-search-kind]")].map((icon) => icon.getAttribute("data-search-kind"))).toEqual(["artist", "series", "tag", "character", "gallery", "post"]);
  expect(panel.querySelector('[data-search-kind="artist"]')).toHaveClass("text-blue-500");
  expect(panel.querySelector('[data-search-kind="series"]')).toHaveClass("text-yellow-600");
  expect(panel.querySelector('[data-search-kind="gallery"]')).toHaveClass("text-green-600");
  expect(panel.querySelector('[data-search-kind="tag"]')).toHaveClass("text-orange-500");
  await user.click(within(panel).getByRole("option", { name: /artist result/ }));
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/artist");
  expect(document.querySelector('[data-ckui="search-panel"]')).toBeNull();
});

it("keeps the default inline panel and content thumbnails", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/inline-cover", fetch: async () => Response.json({ contents: [{ id: "gallery", kind: "gallery", title: "Inline gallery", cover: [{ url: "https://media.test/cover.webp", w: 300, h: 400 }] }], entities: [] }) });
  const { container } = render(<ContentKitProvider client={client} viewer={null}><SearchBox onSearch={() => {}} /></ContentKitProvider>);
  await userEvent.setup().type(screen.getByRole("combobox", { name: "Search" }), "gallery");
  await screen.findByRole("option", { name: "Inline gallery" });
  const panel = container.querySelector('[data-ckui="search-panel"]')!;
  expect(panel).not.toBeNull();
  expect(panel.querySelector("img")).toHaveAttribute("src", "https://media.test/cover.webp");
  expect(document.querySelector('[data-ckui="search-backdrop"]')).toBeNull();
});

it("omits recent dates and keeps portaled history controls working without navigating", async () => {
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
  let width = 400;
  vi.spyOn(input, "getBoundingClientRect").mockImplementation(() => ({ bottom: 48, left: 20, width } as DOMRect));
  await user.click(input);
  await screen.findByRole("option", { name: /older/ });
  expect(document.querySelector("time")).toBeNull();
  width = 240;
  fireEvent.resize(window);
  expect(document.querySelector("time")).toBeNull();
  expect(screen.getByRole("option", { name: /older/ })).toBeVisible();
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

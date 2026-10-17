// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, GroupCard, ListingSummary } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { CardGrid, ContentGrid, ContentRail, SearchResults } from "./grid.js";
import { ContentFavorites } from "./favorites.js";
import { ContentFilterDialog } from "./filter-dialog.js";

const at = "2026-10-16T00:00:00Z";
const entity = (id: string): GroupCard => ({ id, kind: "tag", internal: false, slug: id, name: id.toUpperCase(), count: 0, attributes: {}, created_at: at });
const card = (id: string, allowed = true): Card => ({
  id,
  kind: "gallery",
  code: id,
  path: `/g/${id}`,
  title: `Gallery ${id}`,
  language: "en",
  cover: [],
  version: { id: `${id}-v`, code: id, languages: ["en"], traits: [], has_audio: false },
  entities: {},
  counts: {},
  access: { allowed, level: allowed ? "public" : "tier" },
  attributes: {},
});
const summary = (overrides: Partial<ListingSummary> = {}): ListingSummary => ({
  filtered_count: 5,
  filtered_entities: [
    { entity: entity("futa"), count: 5 },
    { entity: entity("yaoi"), count: 3 },
  ],
  locked_count: 50,
  total: 50,
  ...overrides,
});
function server(items: Card[] | ((request: URL) => Card[]) = [], metadata: ListingSummary = summary()) {
  const reads: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://summary.test",
    fetch: async (input) => {
      const u = new URL(String(input));
      reads.push(u);
      const selected = typeof items === "function" ? items(u) : items;
      if (["/v1/contents", "/v1/search", "/v1/me/history", "/v1/recommendations"].includes(u.pathname) || u.pathname.endsWith("/similar"))
        return Response.json({ contents: selected, summary: metadata });
      if (u.pathname === "/v1/me/favorites") return Response.json({ items: selected.map((content) => ({ content, value: 1, at })), summary: metadata });
      if (u.pathname.endsWith("/content-filters"))
        return Response.json({
          applies_to: ["gallery", "blog"],
          filters: ["futa", "yaoi"].map((id) => ({ entity: entity(id), default_visible: false, visible: false, overridden: false })),
          include_deleted: false,
        });
      if (u.pathname.endsWith("/presets")) return Response.json([{ kind: "gallery", name: "cover", aspect: "7071:10000" }]);
      return Response.json({ code: "not_found", error: "Not found" }, { status: 404 });
    },
  });
  return { client, reads };
}
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("keeps an all-filtered list and opens contextual per-entity counts without adding overlapping totals", async () => {
  const s = server();
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery" }} />
    </ContentKitProvider>,
  );
  const tile = await screen.findByRole("button", { name: "5 items filtered out" });
  expect(screen.queryByText("Nothing matches.")).not.toBeInTheDocument();
  await userEvent.setup().click(tile);
  const dialog = await screen.findByRole("dialog", { name: "Content filters" });
  expect(await within(dialog).findByRole("switch", { name: "FUTA: 5" })).toBeVisible();
  expect(within(dialog).getByRole("switch", { name: "YAOI: 3" })).toBeVisible();
  expect(s.reads.find((u) => u.pathname === "/v1/contents")?.searchParams.get("summary")).toBe("true");
});

it("uses selection-wide locked counts rather than the number of locked cards on the current page", async () => {
  const s = server([card("free"), card("locked", false)]);
  const view = render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery" }} locked="collapse" />
    </ContentKitProvider>,
  );
  expect(await screen.findByRole("button", { name: "Show 50 locked items" })).toBeVisible();
  expect(view.container.querySelector('article[data-id="locked"]')).toBeNull();
  expect(screen.getByRole("button", { name: "5 items filtered out" })).toBeVisible();
  expect(s.reads.some((u) => u.pathname === "/v1/release-dates")).toBe(false);
});

it("marks bounded-search totals and contextual counts as lower bounds", async () => {
  const s = server([], summary({ truncated: true }));
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery" }} />
    </ContentKitProvider>,
  );
  await userEvent.setup().click(await screen.findByRole("button", { name: "≥ 5 items filtered out" }));
  const dialog = await screen.findByRole("dialog", { name: "Content filters" });
  expect(await within(dialog).findByRole("switch", { name: "FUTA: ≥ 5" })).toBeVisible();
});

it("omits a summary tile at zero even when the loaded page has inaccessible metadata", async () => {
  const s = server([card("locked", false)], summary({ filtered_count: 0, filtered_entities: [], locked_count: 0 }));
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery" }} />
    </ContentKitProvider>,
  );
  await screen.findByText("Gallery locked");
  expect(screen.queryByRole("button", { name: /filtered out|locked items/ })).not.toBeInTheDocument();
});

it("clears local premium expansion for a different matching selection", async () => {
  const s = server([card("free"), card("locked", false)], summary({ filtered_count: 0, filtered_entities: [] }));
  const feed = (sort: "newest" | "oldest") => (
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery", sort }} locked="collapse" />
    </ContentKitProvider>
  );
  const view = render(feed("newest"));
  await userEvent.setup().click(await screen.findByRole("button", { name: "Show 50 locked items" }));
  await waitFor(() => expect(view.container.querySelector('article[data-id="locked"]')).not.toBeNull());
  view.rerender(feed("oldest"));
  expect(await screen.findByRole("button", { name: "Show 50 locked items" })).toBeVisible();
  expect(view.container.querySelector('article[data-id="locked"]')).toBeNull();
});

it("does not count an indeterminate access decision as a premium card", async () => {
  const unknown = { ...card("indeterminate", false), access: { ...card("indeterminate", false).access, unknown: true } };
  const s = server([unknown], summary({ filtered_count: 0, filtered_entities: [], locked_count: 0 }));
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentGrid query={{ kind: "gallery" }} locked="collapse" />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("Gallery indeterminate")).toBeVisible();
  expect(screen.queryByRole("button", { name: /locked items/ })).not.toBeInTheDocument();
});

it("applies the homepage preview policy to measured columns and never mounts more than four mobile cards", async () => {
  let mobile = true,
    columns = 2;
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.includes("max-width") && mobile,
    media: query,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() {
      return true;
    },
  }));
  const computed = getComputedStyle;
  vi.spyOn(globalThis, "getComputedStyle").mockImplementation((element, pseudo) => {
    const styles = computed(element, pseudo);
    if (element.tagName === "OL") Object.defineProperty(styles, "gridTemplateColumns", { value: Array.from({ length: columns }, () => "180px").join(" "), configurable: true });
    return styles;
  });
  const s = server();
  const list = {
    items: [card("a"), card("premium", false), ..."bcdefgh".split("").map((id) => card(id))],
    summary: summary(),
    loading: false,
    loadingMore: false,
    hasMore: false,
    loadMore: vi.fn(),
    reload: vi.fn(),
  };
  const view = render(
    <ContentKitProvider client={s.client} viewer={null}>
      <CardGrid list={list} render="home" locked="collapse" lockedSummaryMinColumns={6} showFilteredSummary={false} cardLimit={6} mobileCardLimit={4} />
    </ContentKitProvider>,
  );
  await waitFor(() => expect(view.container.querySelectorAll("article[data-id]")).toHaveLength(4));
  expect(screen.queryByRole("button", { name: /locked items|filtered out/ })).not.toBeInTheDocument();
  mobile = false;
  columns = 6;
  fireEvent.resize(window);
  expect(await screen.findByRole("button", { name: "Show 50 locked items" })).toBeVisible();
  expect(view.container.querySelectorAll("article[data-id]")).toHaveLength(5);
  expect(screen.queryByRole("button", { name: /filtered out/ })).not.toBeInTheDocument();
});

it("measures an all-locked preview before deciding whether its summary tile fits", async () => {
  const computed = getComputedStyle;
  vi.spyOn(globalThis, "getComputedStyle").mockImplementation((element, pseudo) => {
    const styles = computed(element, pseudo);
    if (element.tagName === "OL") Object.defineProperty(styles, "gridTemplateColumns", { value: "180px 180px 180px 180px 180px 180px", configurable: true });
    return styles;
  });
  const s = server();
  const list = {
    items: [card("premium", false)],
    summary: summary({ filtered_count: 0, filtered_entities: [] }),
    loading: false,
    loadingMore: false,
    hasMore: false,
    loadMore: vi.fn(),
    reload: vi.fn(),
  };
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <CardGrid list={list} render="locked-home" locked="collapse" lockedSummaryMinColumns={6} showFilteredSummary={false} />
    </ContentKitProvider>,
  );
  expect(await screen.findByRole("button", { name: "Show 50 locked items" })).toBeVisible();
});

it("shows filter controls for a favorites selection even when every favorite is filtered", async () => {
  const s = server([]);
  render(
    <ContentKitProvider client={s.client} viewer="reader">
      <ContentFavorites query={{ kind: "gallery" }} empty="No favorites yet" />
    </ContentKitProvider>,
  );
  expect(await screen.findByRole("button", { name: "5 items filtered out" })).toBeVisible();
  expect(screen.queryByText("No favorites yet")).toBeNull();
  expect(s.reads.find((u) => u.pathname === "/v1/me/favorites")?.searchParams.get("summary")).toBe("true");
});

it("keeps an entirely filtered related rail visible and uses the shared filter dialog", async () => {
  const s = server([], summary({ truncated: true }));
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentRail title="Related" similar="anchor" query={{ kind: "gallery" }} />
    </ContentKitProvider>,
  );
  const tile = await screen.findByRole("button", { name: "≥ 5 items filtered out" });
  expect(screen.getByRole("heading", { name: "Related" })).toBeVisible();
  await userEvent.setup().click(tile);
  const dialog = await screen.findByRole("dialog", { name: "Content filters" });
  expect(await within(dialog).findByRole("switch", { name: "FUTA: ≥ 5" })).toBeVisible();
  expect(s.reads.find((u) => u.pathname.endsWith("/similar"))?.searchParams.get("summary")).toBe("true");
});

it("uses the shared premium reveal in related rails", async () => {
  const s = server([card("paid", false)], summary({ filtered_count: 0, filtered_entities: [], locked_count: 1 }));
  const view = render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentRail title="Related" similar="anchor" locked="collapse" />
    </ContentKitProvider>,
  );
  expect(view.container.querySelector('article[data-id="paid"]')).toBeNull();
  await userEvent.setup().click(await screen.findByRole("button", { name: "Show 1 locked item" }));
  expect(await screen.findByText("Gallery paid")).toBeVisible();
});

it("asks guests to sign in for favorites without asking the account endpoint", async () => {
  const s = server([card("private")]);
  render(
    <ContentKitProvider client={s.client} viewer={null}>
      <ContentFavorites empty="Favorite galleries will appear here" />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("Sign in to save favorites")).toBeVisible();
  expect(s.reads.some((u) => u.pathname === "/v1/me/favorites")).toBe(false);
});

it.each(["related", "recommended", "list", "history", "favorites", "search"] as const)(
  "applies hide before the %s page limit so a paid first candidate cannot mask a free result",
  async (surface) => {
    // The server's first candidate is paid. A client-only hide would render an
    // empty page; requesting admission before the limit returns the free one.
    const s = server(
      (u) => (u.searchParams.get("mode") === "hide" ? [card("free")] : [card("paid", false)]),
      summary({ filtered_count: 0, filtered_entities: [], locked_count: 0 }),
    );
    const props = { locked: "hide" as const, pageSize: 1 };
    const element =
      surface === "favorites" ? (
        <ContentFavorites {...props} query={{ kind: "gallery" }} />
      ) : surface === "search" ? (
        <SearchResults {...props} query={{ kind: "gallery", q: "example" }} />
      ) : surface === "related" ? (
        <ContentRail {...props} title="Related" similar="anchor" />
      ) : surface === "recommended" ? (
        <ContentRail {...props} title="Recommended" recommendations />
      ) : surface === "history" ? (
        <ContentRail {...props} title="History" history={{ kind: "gallery" }} />
      ) : (
        <ContentRail {...props} title="Latest" list={{ kind: "gallery" }} />
      );
    render(
      <ContentKitProvider client={s.client} viewer="reader">
        {element}
      </ContentKitProvider>,
    );
    expect(await screen.findByText("Gallery free")).toBeVisible();
    expect(screen.queryByText("Gallery paid")).toBeNull();
    expect(s.reads.some((u) => u.searchParams.get("limit") === "1" && u.searchParams.get("mode") === "hide")).toBe(true);
  },
);

it.each([undefined, "show"] as const)("keeps an explicitly supplied hide query when locked is %s", async (locked) => {
  const s = server((u) => (u.searchParams.get("mode") === "hide" ? [card("free")] : [card("paid", false)]), summary({ filtered_count: 0, filtered_entities: [], locked_count: 0 }));
  render(
    <ContentKitProvider client={s.client} viewer="reader">
      <ContentFavorites locked={locked} query={{ mode: "hide" }} />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("Gallery free")).toBeVisible();
  expect(screen.queryByText("Gallery paid")).toBeNull();
});

it("requests locked previews before collapsing them when the supplied query was hide", async () => {
  const s = server((u) => (u.searchParams.get("mode") === "lock" ? [card("paid", false)] : []), summary({ filtered_count: 0, filtered_entities: [], locked_count: 1 }));
  render(
    <ContentKitProvider client={s.client} viewer="reader">
      <ContentFavorites locked="collapse" query={{ mode: "hide" }} />
    </ContentKitProvider>,
  );
  await userEvent.setup().click(await screen.findByRole("button", { name: "Show 1 locked item" }));
  expect(await screen.findByText("Gallery paid")).toBeVisible();
});

it("keeps filter controls inside a native reader dialog instead of its inert background", async () => {
  const s = server([]);
  const view = render(<ContentKitProvider client={s.client} viewer={null}><dialog open aria-label="Gallery reader"><ContentGrid query={{ kind: "gallery" }} /></dialog></ContentKitProvider>);
  const reader = view.container.querySelector("dialog")!;
  await userEvent.setup().click(await screen.findByRole("button", { name: "5 items filtered out" }));
  const filters = await screen.findByRole("dialog", { name: "Content filters" });
  expect(reader).toContainElement(filters);
  expect(await within(filters).findByRole("switch", { name: "FUTA: 5" })).toBeVisible();
  await userEvent.setup().click(within(filters).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Content filters" })).toBeNull());
  expect(reader).toHaveAttribute("open");
});

it("retains the native portal while preferences remove its opening tile", async () => {
  const s = server([]), close = vi.fn();
  const reader = document.createElement("dialog");
  reader.open = true;
  const button = document.createElement("button");
  reader.append(button);
  document.body.append(reader);
  let target: HTMLElement | null = button;
  const content = () => <ContentKitProvider client={s.client} viewer={null}><ContentFilterDialog open onOpenChange={close} returnFocus={() => target} /></ContentKitProvider>;
  const view = render(content());
  try {
    const filters = await screen.findByRole("dialog", { name: "Content filters" });
    expect(reader).toContainElement(filters);
    target = null;
    button.remove();
    view.rerender(content());
    expect(reader).toContainElement(filters);
    await userEvent.setup().click(within(filters).getByRole("button", { name: "Close" }));
    expect(close).toHaveBeenCalledWith(false);
  } finally {
    view.unmount();
    reader.remove();
  }
});

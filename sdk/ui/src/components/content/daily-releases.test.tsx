// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, DailyReleases as DailyReleaseData, EntityCard } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { DailyReleases } from "./daily-releases.js";

const card = (id: string): Card => ({
  id, kind: "gallery", code: id, path: `/en/g/${id}`, title: `Gallery ${id}`, language: "en",
  cover: [{ url: `https://media.test/${id}.webp`, w: 200, h: 300 }],
  version: { id: `${id}-version`, code: id, languages: ["en"], traits: [], has_audio: false },
  entities: {}, counts: {}, access: { allowed: true, level: "public" }, attributes: {},
});
const cards = (prefix: string, length: number) => Array.from({ length }, (_, i) => card(`${prefix}-${i + 1}`));
const data = (): DailyReleaseData => ({ time_zone: "America/Denver", from_date: "2026-10-06", through_date: "2026-10-15", days: [
  { date: "2026-10-15", contents: cards("today", 20), filtered_count: 0, filtered_entities: [] },
  { date: "2026-10-14", contents: cards("yesterday", 15), filtered_count: 0, filtered_entities: [] },
  { date: "2026-10-13", contents: [], filtered_count: 0, filtered_entities: [] },
  { date: "2026-10-12", contents: cards("older", 2), filtered_count: 0, filtered_entities: [] },
] });

afterEach(() => { vi.unstubAllGlobals(); localStorage.clear(); });

it("requests a historical window directly and changes dates without fetching intervening releases", async () => {
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/calendar", fetch: async (input) => {
    const url = new URL(String(input));
    if (!url.pathname.endsWith("/releases")) return Response.json([]);
    requests.push(url);
    const date = url.searchParams.get("end_date") ?? "2026-10-15";
    return Response.json({ time_zone: "America/Denver", from_date: date, through_date: date, days: [{ date, contents: [], filtered_count: 0, filtered_entities: [] }] });
  } });
  const feed = (date?: string) => <ContentKitProvider client={client} viewer={null}><DailyReleases query={{ kind: "gallery", days: 10, end_date: date, time_zone: "America/Denver" }} /></ContentKitProvider>;
  const view = render(feed("2007-04-18"));
  await waitFor(() => expect(requests).toHaveLength(1));
  expect(Object.fromEntries(requests[0]!.searchParams)).toMatchObject({ kind: "gallery", days: "10", end_date: "2007-04-18", time_zone: "America/Denver" });
  view.rerender(feed("1998-12-31"));
  await waitFor(() => expect(requests).toHaveLength(2));
  expect(requests[1]!.searchParams.get("end_date")).toBe("1998-12-31");
  view.rerender(feed());
  await waitFor(() => expect(requests).toHaveLength(3));
  expect(requests[2]!.searchParams.has("end_date")).toBe(false);
});

function observeMedia() {
  const observers: { callback: IntersectionObserverCallback; margin?: string; targets: Set<Element> }[] = [];
  vi.stubGlobal("IntersectionObserver", class {
    private readonly observer;
    constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
      this.observer = { callback, margin: options?.rootMargin, targets: new Set<Element>() };
      observers.push(this.observer);
    }
    observe(element: Element) { this.observer.targets.add(element); }
    unobserve(element: Element) { this.observer.targets.delete(element); }
    disconnect() { this.observer.targets.clear(); }
  });
  return (id: string) => {
    const observer = observers.find((item) => item.margin === "0px" && [...item.targets].some((target) => target.querySelector(`[data-id="${id}"]`)));
    expect(observer).toBeDefined();
    const target = [...observer!.targets][0]!;
    act(() => observer!.callback([{ target, isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver));
  };
}

it("forwards ordered entity rows to daily release cards", async () => {
  observeMedia();
  const item: Card = { ...card("entities"), entities: {
    artist: [{ id: "artist", slug: "artist", name: "An artist" }],
    series: [{ id: "series", slug: "series", name: "A series" }],
  } };
  const client = createContentKitClient({ baseUrl: "https://content.test/entity-rows", fetch: async (input) => Response.json(new URL(String(input)).pathname.endsWith("/presets") ? [] : { ...data(), days: [{ date: "2026-10-15", contents: [item], filtered_count: 0, filtered_entities: [] }] }) });
  const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases entityLayout="rows" entityKinds={["series", "artist"]} /></ContentKitProvider>);
  await screen.findByText("Gallery entities");
  expect([...view.container.querySelectorAll('[data-ckui="card-group-row"]')].map((row) => row.getAttribute("data-kind"))).toEqual(["series", "artist"]);
  expect(within(screen.getByRole("list", { name: "Series" })).getByText("A series")).toBeVisible();
  expect(within(screen.getByRole("list", { name: "Artist" })).getByText("An artist")).toBeVisible();
});

it("fetches all day metadata once, reveals 30 newest cards, and expands cached days without fetching", async () => {
  observeMedia();
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test", fetch: async (input) => {
    requests.push(new URL(String(input)));
    return Response.json(data());
  } });
  const user = userEvent.setup();
  const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases query={{ kind: "gallery", time_zone: "America/Denver" }} grid={{ minCardWidth: 320, mobileColumns: 2 }} aspectRatio="182 / 260" titlePosition="cover" entityStyle="text" /></ContentKitProvider>);
  await screen.findByText("Gallery today-1");
  expect(view.container.querySelectorAll('[data-ckui="content-card"]')).toHaveLength(30);
  expect(view.container.querySelector("ol")?.style.getPropertyValue("--ckui-card-min-width")).toBe("320px");
  expect(view.container.querySelector("ol")?.style.getPropertyValue("--ckui-card-mobile-columns")).toBe("2");
  expect(view.container.querySelector<HTMLElement>('[data-ckui="card-cover-frame"]')?.style.aspectRatio).toBe("182 / 260");
  expect(screen.getByRole("link", { name: "Gallery today-1" }).closest('[data-ckui="card-cover"]')).not.toBeNull();
  expect(screen.queryByText("Gallery yesterday-11")).toBeNull();
  expect(screen.queryByText("Gallery older-1")).toBeNull();
  expect(view.container.querySelectorAll('[data-ckui="release-day"]')).toHaveLength(3);
  const headings = screen.getAllByRole("heading", { level: 2 });
  expect(headings).toHaveLength(3);
  for (const heading of headings) {
    expect(heading.textContent).not.toMatch(/\b\d+ items?\b/);
    expect(heading.textContent).toBe(heading.querySelector("time")?.textContent);
  }
  const request = requests.find((url) => url.pathname === "/v1/releases")!;
  expect(request.searchParams.get("days")).toBe("10");
  expect(request.searchParams.get("kind")).toBe("gallery");
  expect(request.searchParams.get("time_zone")).toBe("America/Denver");

  const yesterday = view.container.querySelector('[data-date="2026-10-14"]') as HTMLElement;
  await user.click(within(yesterday).getByRole("button", { name: "Show more" }));
  expect(screen.getByText("Gallery yesterday-15")).toBeVisible();
  const older = view.container.querySelector('[data-date="2026-10-12"]') as HTMLElement;
  const toggle = within(older).getByRole("button");
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  toggle.focus();
  await user.keyboard("{Enter}");
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByText("Gallery older-2")).toBeVisible();
  expect(requests.filter((url) => url.pathname === "/v1/releases")).toHaveLength(1);
});

it("does not mount offscreen, unrevealed or collapsed card media", async () => {
  const visible = observeMedia();
  const client = createContentKitClient({ baseUrl: "https://content.test/media", fetch: async (input) => Response.json(new URL(String(input)).pathname.endsWith("/presets") ? [] : data()) });
  const user = userEvent.setup();
  const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases query={{ time_zone: "America/Denver" }} /></ContentKitProvider>);
  await screen.findByText("Gallery today-1");
  expect(view.container.querySelectorAll("img[src]")).toHaveLength(0);
  visible("today-1");
  await waitFor(() => expect(view.container.querySelectorAll("img[src]")).toHaveLength(1));
  expect(view.container.querySelector("img")?.getAttribute("src")).toBe("https://media.test/today-1.webp");
  expect(view.container.querySelector("img")).toHaveAttribute("loading", "lazy");
  const today = view.container.querySelector('[data-date="2026-10-15"]') as HTMLElement;
  const toggle = within(today).getByRole("button");
  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(view.container.querySelectorAll("img[src]")).toHaveLength(0);
  expect(today.querySelector('[role="region"]')).toHaveAttribute("inert");
  expect(today.querySelector('[role="region"]')?.className).toContain("motion-reduce:transition-none");
  await user.click(toggle);
  await waitFor(() => expect(view.container.querySelectorAll("img[src]")).toHaveLength(1));
});

it("shows a retryable error and the empty window without per-day requests", async () => {
  let failed = true;
  const fetcher = vi.fn(async () => failed ? Response.json({ code: "unavailable", error: "offline" }, { status: 503 }) : Response.json({ ...data(), days: [] }));
  const client = createContentKitClient({ baseUrl: "https://content.test/retry-daily", fetch: fetcher });
  render(<ContentKitProvider client={client} viewer={null}><DailyReleases /></ContentKitProvider>);
  expect(await screen.findByRole("alert")).toBeVisible();
  failed = false;
  await userEvent.setup().click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Nothing here yet.")).toBeVisible();
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it("refreshes the window when the account changes its content filters", async () => {
  let filtered = false;
  const requests: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/filters", fetch: async (input, init) => {
    const path = new URL(String(input)).pathname;
    if (path.endsWith("/presets")) return Response.json([]);
    requests.push(path);
    if (init?.method === "PATCH") {
      filtered = true;
      return Response.json({ applies_to:["gallery"],filters:[],include_deleted:false });
    }
    return Response.json({ ...data(), days: filtered ? [] : data().days });
  } });
  render(<ContentKitProvider client={client} viewer="reader"><DailyReleases /></ContentKitProvider>);
  await screen.findByText("Gallery today-1");
  await act(async () => { await client.discovery.setContentFilters({ filters:[],include_deleted:false }); });
  expect(await screen.findByText("Nothing here yet.")).toBeVisible();
  expect(requests.filter((path) => path.endsWith("/releases"))).toHaveLength(2);
});

const premium = (id: string, allowed = false): Card => ({ ...card(id), access: { allowed, level: "tier", tiers: ["premium"] } });
const entity = (id: string, name = id): EntityCard => ({ id, slug: id, kind: "tag", internal: false, name, count: 0, attributes: {}, created_at: "2026-01-01T00:00:00Z" });
const windowWith = (contents: Card[], filtered_count = 0, filtered_entities: DailyReleaseData["days"][number]["filtered_entities"] = []): DailyReleaseData => ({ ...data(), days: [{ date: "2026-10-15", contents, filtered_count, filtered_entities }] });

it("collapses only locked tier previews at their first original position and keeps the cached card budget on expansion", async () => {
  const visible = observeMedia();
  const response = windowWith([card("first"), premium("locked-1"), card("middle"), premium("locked-2"), card("last"), ...cards("later", 40)]);
  const requests: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/premium-releases", fetch: async (input) => {
    const path = new URL(String(input)).pathname;
    requests.push(path);
    return Response.json(path.endsWith("/presets") ? [] : response);
  } });
  const user = userEvent.setup();
  const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases collapsePremium initialVisible={5} /></ContentKitProvider>);
  const teaser = await screen.findByRole("button", { name: "2 more items" });
  const grid = teaser.closest("ol")!;
  expect([...grid.children].map((child) => child.getAttribute("data-release-key"))).toEqual(["first", "premium-summary", "middle", "last"]);
  expect(screen.queryByRole("link", { name: /Gallery locked-1/ })).toBeNull();
  expect(view.container.querySelectorAll("img[src]")).toHaveLength(0);
  teaser.focus();
  await user.keyboard("{Enter}");
  expect([...grid.children].map((child) => child.getAttribute("data-release-key"))).toEqual(["first", "locked-1", "middle", "locked-2", "last"]);
  expect(screen.getByRole("link", { name: /Gallery locked-1/ })).toHaveFocus();
  expect(screen.getByRole("link", { name: /Gallery locked-1/ }).closest('[data-ckui="content-card"]')).toHaveAttribute("data-locked");
  expect(view.container.querySelectorAll('[data-ckui="content-card"]')).toHaveLength(5);
  expect(screen.queryByText("Gallery later-1")).toBeNull();
  expect(screen.getByRole("button", { name: "Show more" })).toBeVisible();
  expect(view.container.querySelectorAll("img[src]")).toHaveLength(0);
  visible("locked-1");
  await waitFor(() => expect(view.container.querySelectorAll("img[src]")).toHaveLength(1));
  // Image completion never changes the number or order of revealed cards.
  view.container.querySelector("img")!.dispatchEvent(new Event("load"));
  expect(grid.children).toHaveLength(5);
  expect(requests.filter((path) => path.endsWith("/releases"))).toHaveLength(1);
});

it("uses each card's access decision for signed-in readers and leaves allowed premium content inline", async () => {
  observeMedia();
  const client = createContentKitClient({ baseUrl: "https://content.test/allowed-premium", fetch: async (input) => Response.json(String(input).endsWith("/presets") ? [] : windowWith([premium("allowed", true), card("public")])) });
  render(<ContentKitProvider client={client} viewer="member"><DailyReleases collapsePremium /></ContentKitProvider>);
  expect(await screen.findByRole("link", { name: "Gallery allowed" })).toBeVisible();
  expect(screen.queryByText(/more items/)).toBeNull();
  expect(screen.queryByRole("button", { name: /filtered out/ })).toBeNull();
});

it("keeps an all-filtered day and shows its distinct total with overlapping per-entity counts, omitting unoffered entities", async () => {
  observeMedia();
  const futa = entity("futanari", "Futa");
  const gore = entity("gore", "Gore");
  const response = windowWith([], 5, [
    { entity: futa, count: 5 },
    { entity: gore, count: 3 },
    { entity: entity("internal-secret"), count: 99 },
  ]);
  const client = createContentKitClient({ baseUrl: "https://content.test/filtered-only", fetch: async (input) => Response.json(String(input).includes("content-filters") ? { applies_to:["gallery"],filters:[futa,gore].map(item=>({entity:item,default_visible:false,visible:false,overridden:false})),include_deleted:false } : String(input).endsWith("/presets") ? [] : response) });
  render(<ContentKitProvider client={client} viewer={null}><DailyReleases query={{ kind: "gallery" }} /></ContentKitProvider>);
  await userEvent.setup().click(await screen.findByRole("button", { name: "5 items filtered out" }));
  const dialog = await screen.findByRole("dialog", { name: "Content filters" });
  expect(within(dialog).getByRole("switch", { name: "Futa: 5" })).not.toBeChecked();
  expect(within(dialog).getByRole("switch", { name: "Gore: 3" })).not.toBeChecked();
  expect(within(dialog).queryByText(/internal-secret/)).toBeNull();
  expect(within(dialog).getByText(/October 15, 2026/)).toBeVisible();
  expect(within(dialog).queryByRole("combobox", { name: "Language" })).toBeNull();
});

it.each([null, "reader"])("updates day counts after a %s preference change without closing the dialog or using global totals", async (viewer) => {
  observeMedia();
  const futa = entity("futanari", "Futa");
  let allowed = false;
  let writes = 0;
  let releaseReads = 0;
  const client = createContentKitClient({ baseUrl: `https://content.test/filter-update-${viewer ?? "guest"}`, fetch: async (input, init) => {
    const path = new URL(String(input)).pathname;
    if (init?.method === "PATCH") { writes++; allowed = true; }
    if (path.includes("content-filters")) return Response.json({ applies_to:["gallery"],filters:[{entity:futa,default_visible:false,visible:allowed,overridden:allowed}],include_deleted:false });
    if (path.endsWith("/presets")) return Response.json([]);
    releaseReads++;
    const guestAllowed = new Headers(init?.headers).get("ContentKit-Reader-Preferences")?.includes('"visible":true');
    const shown = allowed || guestAllowed;
    return Response.json(shown ? windowWith([card("now-visible")]) : windowWith([], 5, [{ entity: futa, count: 5 }]));
  } });
  const user = userEvent.setup();
  render(<ContentKitProvider client={client} viewer={viewer}><DailyReleases query={{ kind: "gallery" }} /></ContentKitProvider>);
  await user.click(await screen.findByRole("button", { name: "5 items filtered out" }));
  await user.click(await screen.findByRole("switch", { name: "Futa: 5" }));
  await waitFor(() => expect(screen.getByRole("switch", { name: "Futa: 0" })).toBeChecked());
  expect(screen.getByRole("dialog", { name: "Content filters" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "5 items filtered out" })).toBeNull();
  expect(releaseReads).toBe(2);
  expect(writes).toBe(viewer ? 1 : 0);
  if (!viewer) expect(client.preferences.browser.snapshot().filters).toEqual([{ entity_id: "futanari", visible: true }]);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.getByRole("heading", { level: 2 }).querySelector("button")).toHaveFocus();
});

it("uses bounded native reveal animations and disables them for reduced motion", async () => {
  observeMedia();
  const animate = vi.fn<Element["animate"]>(() => ({ cancel() { /* The test records keyframes, not a browser timeline. */ } } as Animation));
  const original = Element.prototype.animate;
  Element.prototype.animate = animate;
  try {
    const client = createContentKitClient({ baseUrl: "https://content.test/animated-reveal", fetch: async (input) => Response.json(String(input).endsWith("/presets") ? [] : windowWith([premium("one"), premium("two")])) });
    const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases collapsePremium /></ContentKitProvider>);
    await userEvent.setup().click(await screen.findByRole("button", { name: "2 more items" }));
    expect(animate).toHaveBeenCalledTimes(2);
    expect(animate.mock.calls.map(([, options]) => options)).toEqual([
      expect.objectContaining({ duration: 400, delay: 0 }), expect.objectContaining({ duration: 400, delay: 45 }),
    ]);
    view.unmount();
    animate.mockClear();
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener() {}, removeEventListener() {} }));
    const reducedClient = createContentKitClient({ baseUrl: "https://content.test/reduced-reveal", fetch: async (input) => Response.json(String(input).endsWith("/presets") ? [] : windowWith([premium("one"), premium("two")])) });
    render(<ContentKitProvider client={reducedClient} viewer={null}><DailyReleases collapsePremium /></ContentKitProvider>);
    await userEvent.setup().click(await screen.findByRole("button", { name: "2 more items" }));
    expect(animate).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: /Gallery one/ })).toBeVisible();
  } finally { Element.prototype.animate = original; }
});

it("does not carry an open day dialog or prior cards across an account change", async () => {
  observeMedia();
  const futa = entity("futanari", "Futa");
  let hold = false;
  const client = createContentKitClient({ baseUrl: "https://content.test/account-isolation", fetch: async (input, init) => {
    const path = new URL(String(input)).pathname;
    if (path.includes("content-filters")) return Response.json({ applies_to:["gallery"],filters:[{entity:futa,default_visible:false,visible:false,overridden:false}],include_deleted:false });
    if (path.endsWith("/presets")) return Response.json([]);
    if (hold) return new Promise<Response>((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true }));
    return Response.json(windowWith([card("alice-private-standing")], 5, [{ entity: futa, count: 5 }]));
  } });
  const view = render(<ContentKitProvider client={client} viewer="alice"><DailyReleases /></ContentKitProvider>);
  await userEvent.setup().click(await screen.findByRole("button", { name: "5 items filtered out" }));
  expect(await screen.findByRole("dialog", { name: "Content filters" })).toBeVisible();
  hold = true;
  view.rerender(<ContentKitProvider client={client} viewer="bob"><DailyReleases /></ContentKitProvider>);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.queryByText("Gallery alice-private-standing")).toBeNull();
});

const summaryAspect = (element: HTMLElement): number => {
  const [width, height = "1"] = element.style.aspectRatio.split("/");
  return Number(width) / Number(height);
};

it("matches the requested kind's cover aspect for a filtered-only day", async () => {
  observeMedia();
  const requests: string[] = [];
  const response = windowWith([], 2, [{ entity: entity("futanari", "Futa"), count: 2 }]);
  const client = createContentKitClient({ baseUrl: "https://content.test/filtered-cover-aspect", fetch: async (input) => {
    const path = new URL(String(input)).pathname;
    requests.push(path);
    return Response.json(path.endsWith("/presets") ? [{ kind: "gallery", name: "cover", aspect: "7071:10000" }] : response);
  } });
  render(<ContentKitProvider client={client} viewer={null}><DailyReleases query={{ kind: "gallery" }} /></ContentKitProvider>);
  const tile = await screen.findByRole("button", { name: "2 items filtered out" });
  await waitFor(() => expect(summaryAspect(tile)).toBeCloseTo(0.7071, 5));
  expect(requests.filter((path) => path.endsWith("/presets"))).toHaveLength(1);
  expect(requests.filter((path) => path.endsWith("/releases"))).toHaveLength(1);
});

it.each([
  { name: "shared filtered selection without requested kind", contents: [], filterKind: "gallery", queryKind: undefined, explicit: undefined, expected: 0.75 },
  { name: "visible cards cannot imply omitted content kind", contents: [card("visible")], filterKind: undefined, queryKind: undefined, explicit: undefined, expected: 0.75 },
  { name: "explicit requested kind", contents: [], filterKind: "blog", queryKind: "gallery", explicit: undefined, expected: 0.7071 },
  { name: "mixed content kinds", contents: [card("visible")], filterKind: "blog", queryKind: undefined, explicit: undefined, expected: 0.75 },
  { name: "absent kind metadata", contents: [], filterKind: undefined, queryKind: undefined, explicit: undefined, expected: 0.75 },
  { name: "explicit card aspect", contents: [], filterKind: "gallery", queryKind: "gallery", explicit: "1 / 1", expected: 1 },
])("uses safe summary aspect selection for $name", async ({ name, contents, filterKind, queryKind, explicit, expected }) => {
  observeMedia();
  const response = windowWith(contents, 2, filterKind ? [{ entity: entity("filter"), count: 2 }] : []);
  const client = createContentKitClient({ baseUrl: `https://content.test/summary-aspect-${name.replaceAll(" ", "-")}`, fetch: async (input) => Response.json(String(input).endsWith("/presets") ? [{ kind: "gallery", name: "cover", aspect: "7071:10000" }, { kind: "blog", name: "cover", aspect: "16:9" }] : response) });
  render(<ContentKitProvider client={client} viewer={null}><DailyReleases query={{ kind: queryKind }} aspectRatio={explicit} /></ContentKitProvider>);
  const tile = await screen.findByRole("button", { name: "2 items filtered out" });
  await waitFor(() => expect(summaryAspect(tile)).toBeCloseTo(expected, 5));
});

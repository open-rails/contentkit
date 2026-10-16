// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, DailyReleases as DailyReleaseData } from "../../client/generated/wire.js";
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
  { date: "2026-10-15", contents: cards("today", 20) },
  { date: "2026-10-14", contents: cards("yesterday", 15) },
  { date: "2026-10-13", contents: [] },
  { date: "2026-10-12", contents: cards("older", 2) },
] });

afterEach(() => { vi.unstubAllGlobals(); localStorage.clear(); });

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
  const client = createContentKitClient({ baseUrl: "https://content.test/entity-rows", fetch: async (input) => Response.json(new URL(String(input)).pathname.endsWith("/presets") ? [] : { ...data(), days: [{ date: "2026-10-15", contents: [item] }] }) });
  const view = render(<ContentKitProvider client={client} viewer={null}><DailyReleases entityLayout="rows" groupKinds={["series", "artist"]} /></ContentKitProvider>);
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
    if (init?.method === "PUT") {
      filtered = true;
      return Response.json({ kinds: { gallery: [] }, include_deleted: false });
    }
    return Response.json({ ...data(), days: filtered ? [] : data().days });
  } });
  render(<ContentKitProvider client={client} viewer="reader"><DailyReleases /></ContentKitProvider>);
  await screen.findByText("Gallery today-1");
  await act(async () => { await client.discovery.setContentFilters({ kinds: { gallery: [] }, include_deleted: false }); });
  expect(await screen.findByText("Nothing here yet.")).toBeVisible();
  expect(requests.filter((path) => path.endsWith("/releases"))).toHaveLength(2);
});

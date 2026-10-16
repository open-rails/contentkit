// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { PopularContentGrid } from "./popular.js";

const card = (id: string): Card => ({ id, kind: "gallery", code: id, path: `/${id}`, title: `Gallery ${id}`, language: "en", cover: [], version: { id, code: id, languages: ["en"], traits: [], has_audio: false }, entities: {}, counts: {}, access: { allowed: true, level: "public" }, attributes: {} });
const model = { languages: ["en"], tiers: [], kinds: {}, entity_kinds: {} };
afterEach(() => { vi.unstubAllGlobals(); localStorage.clear(); });

it("selects the activity window without excluding older publications", async () => {
  const requests: URL[] = [];
  const changed = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/popular", fetch: async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    requests.push(url);
    return Response.json({ contents: [card(url.searchParams.get("sort")?.split(":")[1] ?? "missing")] });
  } });
  render(<ContentKitProvider client={client} viewer={null}><PopularContentGrid query={{ kind: "gallery" }} onPeriodChange={changed} /></ContentKitProvider>);
  await screen.findByText("Gallery 7d");
  expect(screen.getByRole("tablist", { name: "Popularity period" })).toBeVisible();
  expect(screen.getAllByRole("tab")).toHaveLength(4);
  expect(screen.getByRole("tab", { name: "Past 7 days" })).toHaveAttribute("aria-selected", "true");
  expect(screen.queryByRole("combobox", { name: "Popularity period" })).toBeNull();
  const user = userEvent.setup();
  for (const [period, label] of [["90d", "Past 90 days"], ["365d", "Past 365 days"], ["all", "all time"]]) {
    await user.click(screen.getByRole("tab", { name: label }));
    expect(screen.getByRole("tab", { name: label })).toHaveAttribute("aria-selected", "true");
    await screen.findByText(`Gallery ${period}`);
    const request = requests.filter((url) => url.pathname.endsWith("/contents")).at(-1)!;
    expect(request.searchParams.get("sort")).toBe(`popular:${period}`);
    expect(request.searchParams.has("published_within")).toBe(false);
    expect(changed).toHaveBeenLastCalledWith(period);
  }
  expect(screen.queryByRole("combobox", { name: "Language" })).toBeNull();
});

it("loads the next cursor when the shared grid's end reaches the viewport", async () => {
  const observers: { callback: IntersectionObserverCallback; margin?: string }[] = [];
  vi.stubGlobal("IntersectionObserver", class {
    constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) { observers.push({ callback, margin: options?.rootMargin }); }
    observe() {}
    disconnect() {}
  });
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/scroll", fetch: async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    requests.push(url);
    return Response.json(url.searchParams.has("cursor") ? { contents: [card("second")] } : { contents: [card("first")], next_cursor: "next-page" });
  } });
  render(<ContentKitProvider client={client} viewer={null}><PopularContentGrid /></ContentKitProvider>);
  await screen.findByText("Gallery first");
  await waitFor(() => expect(observers.some((observer) => observer.margin === "600px 0px")).toBe(true));
  act(() => observers.filter((observer) => observer.margin === "600px 0px").forEach((observer) => observer.callback([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver)));
  expect(await screen.findByText("Gallery second")).toBeVisible();
  const request = requests.find((url) => url.searchParams.has("cursor"))!;
  expect(request.searchParams.get("cursor")).toBe("next-page");
  expect(request.searchParams.get("sort")).toBe("popular:7d");
  expect(request.searchParams.has("published_within")).toBe(false);
});

it("allows an explicit publication-age filter independently of the activity window", async () => {
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/recent-popular", fetch: async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    requests.push(url);
    return Response.json({ contents: [card("older-window")] });
  } });
  render(<ContentKitProvider client={client} viewer={null}><PopularContentGrid period="7d" query={{ published_within: "90d" }} /></ContentKitProvider>);
  await screen.findByText("Gallery older-window");
  const request = requests.find((url) => url.pathname.endsWith("/contents"))!;
  expect(request.searchParams.get("sort")).toBe("popular:7d");
  expect(request.searchParams.get("published_within")).toBe("90d");
});

// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { beaconsFor, useExposure } from "./discovery.js";
import { ContentKitProvider } from "./provider.js";

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("emits cumulative rendered and visible snapshots with increasing revisions", () => {
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  const callbacks: IntersectionObserverCallback[] = [];
  vi.stubGlobal("IntersectionObserver", class {
    constructor(callback: IntersectionObserverCallback) { callbacks.push(callback); }
    observe() {} unobserve() {} disconnect() {}
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/exposures" });
  const exposure = vi.spyOn(beaconsFor(client), "exposure").mockImplementation(() => {});
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(({ items }) => useExposure(items, { surface: "popular", render: "page" }), { wrapper, initialProps: { items: [{ id: "a" }] } });
  const a = document.createElement("div"), b = document.createElement("div");
  result.current.track("a", 1)(a);
  act(() => { callbacks.at(-1)!([{ target: a, isIntersecting: true } as unknown as IntersectionObserverEntry], {} as IntersectionObserver); vi.advanceTimersByTime(300); });
  vi.setSystemTime(Date.now() + 1000);
  rerender({ items: [{ id: "a" }, { id: "b" }] });
  result.current.track("b", 2)(b);
  act(() => { callbacks.at(-1)!([{ target: b, isIntersecting: true } as unknown as IntersectionObserverEntry], {} as IntersectionObserver); vi.advanceTimersByTime(300); });
  for (const stage of ["rendered", "visible"]) {
    const calls = exposure.mock.calls.map(([event]) => event).filter((event) => event.stage === stage);
    expect(calls.map((event) => event.revision)).toEqual([1, 2]);
    expect(calls[0]?.shown).toEqual([{ content_id: "a", position: 1 }]);
    expect(calls[1]?.shown).toEqual([{ content_id: "a", position: 1 }, { content_id: "b", position: 2 }]);
    expect(calls[1]?.occurred_at).toBe(calls[0]?.occurred_at);
  }
});



it("gives another account a new render identity and rejects stale exposure callbacks", () => {
  let viewer = "alice";
  const client = createContentKitClient({ baseUrl: "https://content.test/exposure-identity" });
  const batcher = beaconsFor(client);
  vi.spyOn(batcher, "exposure").mockImplementation(() => {});
  const click = vi.spyOn(batcher, "click").mockImplementation(() => {});
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useExposure([{ id: "one" }], { surface: "home", render: "same-query" }), { wrapper });
  const old = result.current;
  viewer = "bob";
  rerender();
  expect(result.current.render_id).not.toBe(old.render_id);
  old.click("one", 1);
  expect(click).not.toHaveBeenCalled();
  result.current.click("one", 1);
  expect(click).toHaveBeenCalledExactlyOnceWith("one", expect.objectContaining({ render_id: result.current.render_id }));
});

it("partitions long lists into bounded cumulative renders and binds clicks to the correct segment", () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/segments" });
  const batcher = beaconsFor(client);
  const exposure = vi.spyOn(batcher, "exposure").mockImplementation(() => {});
  const click = vi.spyOn(batcher, "click").mockImplementation(() => {});
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const items = Array.from({ length: 501 }, (_, i) => ({ id: `item-${i}`, version: { id: `v-${i}` } }));
  const { result, rerender } = renderHook(({ items }) => useExposure(items, { surface: "popular", render: "long-list" }), { wrapper, initialProps: { items } });
  expect(exposure.mock.calls.map(([e]) => e.shown.length)).toEqual([...Array<number>(15).fill(32), 21]);
  const renders = exposure.mock.calls.map(([e]) => e.render_id);
  expect(new Set(renders).size).toBe(16);
  result.current.click("item-400", 401);
  expect(click).toHaveBeenCalledWith("item-400", expect.objectContaining({ render_id: renders[12], position: 401, version_id: "v-400" }));
  rerender({ items: [...items, { id: "item-501", version: { id: "v-501" } }] });
  const last = exposure.mock.calls.at(-1)![0];
  expect(last.render_id).toBe(renders[15]);
  expect(last.revision).toBe(2);
  expect(last.shown).toHaveLength(22);
});

it("uses the provider's selected language for list receipts", async () => {
  const languages: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/provider-language", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) { languages.push(JSON.parse(String(init?.body)).language); return Response.json({ enabled: false, contexts: [] }); }
    return new Response(null, { status: 204 });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice" language="fr">{children}</ContentKitProvider>;
  renderHook(() => useExposure([{ id: "content" }], { surface: "home", render: "list" }), { wrapper });
  await act(async () => { await beaconsFor(client).flush(); });
  expect(languages).toEqual(["fr"]);
});


it("preserves already observed receipts across StrictMode effect replay", async () => {
  const targets: string[][] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/strict-exposure", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) {
      const body = JSON.parse(String(init?.body));
      targets.push(body.targets.map((t: { content_id: string }) => t.content_id));
      return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "receipt", occurred_at: "2026-10-15T12:00:00Z" })) });
    }
    return new Response(null, { status: 204 });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <StrictMode><ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider></StrictMode>;
  const { rerender } = renderHook(({ items }) => useExposure(items, { surface: "home", render: "list" }), { wrapper, initialProps: { items: [{ id: "one" }] } });
  await act(async () => { await beaconsFor(client).flush(); });
  rerender({ items: [{ id: "one" }, { id: "two" }] });
  await act(async () => { await beaconsFor(client).flush(); });
  expect(targets).toEqual([["one"], ["two"]]);
});

it("reports only shown cards while preserving original rank gaps for exposures and clicks", () => {
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  const callbacks: IntersectionObserverCallback[] = [];
  vi.stubGlobal("IntersectionObserver", class {
    constructor(callback: IntersectionObserverCallback) { callbacks.push(callback); }
    observe() {} unobserve() {} disconnect() {}
  });
  const client = createContentKitClient({ baseUrl: "https://content.test/rank-gaps" });
  const batcher = beaconsFor(client);
  const exposure = vi.spyOn(batcher, "exposure").mockImplementation(() => {});
  const click = vi.spyOn(batcher, "click").mockImplementation(() => {});
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result } = renderHook(() => useExposure([{ id: "first", position: 1 }, { id: "third", position: 3 }], { client, surface: "home", render: "cards" }), { wrapper });
  const third = document.createElement("div");
  result.current.track("third", 3)(third);
  act(() => {
    callbacks.at(-1)!([{ target: third, isIntersecting: true } as unknown as IntersectionObserverEntry], {} as IntersectionObserver);
    vi.advanceTimersByTime(300);
  });
  const rendered = exposure.mock.calls.map(([event]) => event).find((event) => event.stage === "rendered");
  const visible = exposure.mock.calls.map(([event]) => event).findLast((event) => event.stage === "visible");
  expect(rendered?.shown).toEqual([{ content_id: "first", position: 1 }, { content_id: "third", position: 3 }]);
  expect(visible?.shown).toEqual([{ content_id: "third", position: 3 }]);
  result.current.click("third", 3);
  expect(click).toHaveBeenCalledExactlyOnceWith("third", expect.objectContaining({ render_id: rendered?.render_id, position: 3 }));
});

// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Rendition } from "../client/rendition.js";
import { RenditionImg } from "./rendition-img.js";

let width = 700;
const cached = new Set<string>();
const observers: { measure: () => void; disconnect: ReturnType<typeof vi.fn> }[] = [];
class Preload {
  src = "";
  complete = false;
  naturalWidth = 0;
  onload: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  constructor() { preloads.push(this); }
}
const preloads: Preload[] = [];
const outputs: Rendition[] = [
  { url: "https://media.test/small.webp", w: 100, h: 140 },
  { url: "https://media.test/medium.webp", w: 300, h: 420 },
  { url: "https://media.test/large.webp", w: 800, h: 1120 },
];
const image = () => screen.getByRole("img", { name: "Cover" }) as HTMLImageElement;
const resize = (next: number) => act(() => { width = next; for (const o of [...observers]) o.measure(); });
const cover = (items = outputs, key = "cover") => <RenditionImg key={key} alt="Cover" outputs={items} density={[1, 1]} skeleton fallback={<span>Unavailable</span>} />;

beforeEach(() => {
  width = 700;
  cached.clear();
  preloads.length = 0;
  observers.length = 0;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(() => new DOMRect(0, 0, width, 100));
  vi.spyOn(HTMLImageElement.prototype, "complete", "get").mockImplementation(function (this: HTMLImageElement) { return cached.has(this.getAttribute("src") ?? ""); });
  vi.spyOn(HTMLImageElement.prototype, "naturalWidth", "get").mockImplementation(function (this: HTMLImageElement) { return cached.has(this.getAttribute("src") ?? "") ? 100 : 0; });
  vi.stubGlobal("Image", Preload);
  vi.stubGlobal("ResizeObserver", class {
    private item;
    constructor(measure: () => void) { this.item = { measure, disconnect: vi.fn() }; observers.push(this.item); }
    observe() {}
    disconnect() { this.item.disconnect(); }
  });
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("tries only the smallest smaller rendition and recovers a repaired selected URL without changing that smallest URL", () => {
  const view = render(cover());
  expect(image()).toHaveAttribute("src", outputs[2]!.url);
  fireEvent.error(image());
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  fireEvent.load(image());
  const repaired = outputs.map((o, i) => i === 2 ? { ...o, url: "https://media.test/large-repaired.webp" } : o);
  view.rerender(cover(repaired));
  expect(image()).toHaveAttribute("src", repaired[2]!.url);
  fireEvent.load(image());
  expect(image()).not.toHaveAttribute("data-loading");
});

it("stops after the single smaller fallback fails, without trying every rendition on rerenders or resize", () => {
  const view = render(cover());
  fireEvent.error(image());
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  fireEvent.error(image());
  expect(screen.getByText("Unavailable")).toBeVisible();
  for (let i = 0; i < 4; i++) { view.rerender(cover(outputs.map((o) => ({ ...o })))); resize(700 + i); }
  expect(screen.queryByRole("img")).toBeNull();
  expect(preloads).toHaveLength(0);
});

it("recovers a changed single output and allows one deliberate remount retry of an unchanged source", () => {
  const view = render(cover([outputs[0]!]));
  fireEvent.error(image());
  view.rerender(cover([outputs[0]!]));
  expect(screen.getByText("Unavailable")).toBeVisible();
  view.rerender(cover([outputs[0]!], "explicit-retry"));
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  fireEvent.error(image());
  const repaired = { ...outputs[0]!, url: "https://media.test/small-new-generation.webp" };
  view.rerender(cover([repaired], "explicit-retry"));
  expect(image()).toHaveAttribute("src", repaired.url);
});

it("settles a cached image before paint and keeps it visible while a larger rendition loads", () => {
  width = 80;
  cached.add(outputs[0]!.url);
  render(cover());
  expect(image()).not.toHaveAttribute("data-loading");
  resize(700);
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  expect(preloads).toHaveLength(1);
  act(() => preloads[0]!.onload?.(new Event("load")));
  expect(image()).toHaveAttribute("src", outputs[2]!.url);
});

it("does not repeat a failed background upgrade for cloned outputs, but accepts a genuinely new upgrade URL", () => {
  width = 80;
  const view = render(cover());
  fireEvent.load(image());
  resize(700);
  expect(preloads).toHaveLength(1);
  act(() => preloads[0]!.onerror?.(new Event("error")));
  view.rerender(cover(outputs.map((o) => ({ ...o }))));
  resize(710);
  expect(preloads).toHaveLength(1);
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  const repaired = outputs.map((o, i) => i === 2 ? { ...o, url: "https://media.test/large-generation2.webp" } : o);
  view.rerender(cover(repaired));
  expect(preloads).toHaveLength(2);
  act(() => preloads[1]!.onload?.(new Event("load")));
  expect(image()).toHaveAttribute("src", repaired[2]!.url);
});

it("disconnects resize and preload listeners, and ignores an old completion after outputs change or unmount", () => {
  width = 80;
  const view = render(cover());
  fireEvent.load(image());
  resize(700);
  const old = preloads[0]!;
  const lateLoad = old.onload;
  const repaired = outputs.map((o, i) => i === 2 ? { ...o, url: "https://media.test/large-current.webp" } : o);
  view.rerender(cover(repaired));
  expect(old.onload).toBeNull();
  expect(old.onerror).toBeNull();
  act(() => lateLoad?.(new Event("load")));
  expect(image()).toHaveAttribute("src", outputs[0]!.url);
  const active = preloads[1]!;
  view.unmount();
  expect(active.onload).toBeNull();
  expect(active.onerror).toBeNull();
  expect(observers.every((o) => o.disconnect.mock.calls.length === 1)).toBe(true);
});

it("ignores events from a replaced DOM image after its new source has loaded", () => {
  const view = render(cover([outputs[0]!]));
  const old = image();
  fireEvent.load(old);
  const next = { ...outputs[0]!, url: "https://media.test/small-current.webp" };
  view.rerender(cover([next]));
  const current = image();
  fireEvent.load(current);
  fireEvent.error(old);
  fireEvent.load(old);
  expect(image()).toBe(current);
  expect(image()).not.toHaveAttribute("data-loading");
});

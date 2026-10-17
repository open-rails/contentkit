// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { usePostReading } from "../../react/post-reads.js";
import { PostList, excerpt } from "./post-list.js";

const later = new Date(Date.now() + 3_600_000).toISOString();
const card = (id: string, published_at: string, extra: Partial<Card> = {}): Card => ({
  id, kind: "blog", code: id, path: `/en/blog/${id}`, title: `Post ${id}`, language: "en", cover: [], published_at,
  version: { id, code: id, languages: ["en"], traits: [], has_audio: false }, entities: {}, counts: {}, access: { allowed: true, level: "public" }, attributes: {}, ...extra,
});
const model = { languages: ["en"], tiers: ["premium"], kinds: {}, entity_kinds: {} };
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });

function client(trending: Card[] = [], contents?: Card[]) {
  const lists: URL[] = [];
  const details: URL[] = [];
  const c = createContentKitClient({ baseUrl: "https://content.test/blog", fetch: async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    if (url.pathname.endsWith("/media/presets")) return Response.json([]);
    if (url.pathname.endsWith("/activity-contexts")) return Response.json({ enabled: false, contexts: [] });
    if (/\/contents\/[^/]+$/.test(url.pathname)) details.push(url);
    lists.push(url);
    if (url.searchParams.get("sort")?.startsWith("popular")) return Response.json({ contents: trending });
    if (url.searchParams.get("cursor")) return Response.json({ contents: [card("more", "2020-01-01T00:00:00Z", { excerpt: "Second page." })] });
    const locked = card("old", "2020-01-01T00:00:00Z", { access: { allowed: false, level: "tier", tiers: ["premium"] } });
    return Response.json({ contents: contents ?? [card("fresh", later, { excerpt: "A short summary." }), locked], next_cursor: url.searchParams.get("limit") === "2" ? "c2" : undefined });
  } });
  return { c, lists, details };
}

it("lists posts with their server excerpts, marks posts published since a guest's first visit unread and adds a newest sidebar", async () => {
  const { c, lists, details } = client();
  render(<ContentKitProvider client={c} viewer={null}><PostList kind="blog" pageSize={2} tiers={["premium"]} /></ContentKitProvider>);
  expect(await screen.findByText("A short summary.")).toBeVisible();
  const fresh = document.querySelector('[data-ckui="post-row"][data-id="fresh"]') as HTMLElement;
  const old = document.querySelector('[data-ckui="post-row"][data-id="old"]') as HTMLElement;
  expect(within(fresh).getByText("Unread")).toBeInTheDocument();
  expect(within(old).queryByText("Unread")).toBeNull();
  expect(old.querySelector('[data-ckui="card-lock"]')).not.toBeNull();
  const sidebar = document.querySelector('[data-ckui="post-list-sidebar"]') as HTMLElement;
  expect(within(sidebar).getByRole("heading", { name: "Newest" })).toBeVisible();
  // Trending without ranked posts leaves its section out; it never asks for views.
  await waitFor(() => expect(lists.some((u) => u.searchParams.get("sort") === "popular:30d")).toBe(true));
  expect(within(sidebar).queryByRole("heading", { name: "Trending" })).toBeNull();

  await userEvent.setup().click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("Second page.")).toBeVisible();
  expect(lists.some((u) => u.searchParams.get("cursor") === "c2")).toBe(true);
  // Excerpts come with the cards: no per-post detail request.
  expect(details).toEqual([]);
});

it("marks a signed-in reader's recent posts unread until their measured history has them", async () => {
  const recent = new Date(Date.now() - 2 * 86_400_000).toISOString();
  const stale = new Date(Date.now() - 60 * 86_400_000).toISOString();
  const { c } = client([], [
    card("unseen", recent, { viewer: { favorited: false, seen: false, completed: false, progress: 0, progress_max: 0 } }),
    card("seen", recent, { viewer: { favorited: false, seen: true, completed: false, progress: 1, progress_max: 1 } }),
    card("stale", stale, { viewer: { favorited: false, seen: false, completed: false, progress: 0, progress_max: 0 } }),
  ]);
  render(<ContentKitProvider client={c} viewer="reader"><PostList kind="blog" sidebarSize={0} /></ContentKitProvider>);
  await screen.findByText("Post unseen");
  const row = (id: string) => document.querySelector(`[data-ckui="post-row"][data-id="${id}"]`) as HTMLElement;
  expect(within(row("unseen")).getByText("Unread")).toBeInTheDocument();
  expect(within(row("seen")).queryByText("Unread")).toBeNull();
  expect(within(row("stale")).queryByText("Unread")).toBeNull();
});

it("shows trending posts and drops a guest's unread mark once the post was attended, not merely opened", async () => {
  const { c } = client([card("hot", "2021-01-01T00:00:00Z", { title: "Hot post" })]);
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  let attend = false;
  function Read({ id }: { id: string }) {
    const ref = usePostReading(id, undefined, { guestReadAfter: 1000 });
    return <article ref={(element) => {
      if (element) Object.defineProperty(element, "getBoundingClientRect", { configurable: true, value: () => ({ top: 0, bottom: attend ? 500 : -10, height: 500 }) });
      ref(element);
    }} />;
  }
  render(<ContentKitProvider client={c} viewer={null}><Read id="fresh" /><PostList kind="blog" /></ContentKitProvider>);
  const sidebar = await waitFor(() => document.querySelector('[data-ckui="post-list-sidebar"]') as HTMLElement);
  expect(await within(sidebar).findByRole("heading", { name: "Trending" })).toBeVisible();
  expect(within(sidebar).getByText("Hot post")).toBeVisible();
  await screen.findByText("A short summary.");
  const fresh = () => document.querySelector('[data-ckui="post-row"][data-id="fresh"]') as HTMLElement;
  // Off screen, the open page is not a read.
  await act(() => new Promise((r) => setTimeout(r, 1200)));
  expect(within(fresh()).getByText("Unread")).toBeInTheDocument();
  attend = true;
  await waitFor(() => expect(within(fresh()).queryByText("Unread")).toBeNull(), { timeout: 3000 });
});

it("cuts excerpts at a word boundary", () => {
  expect(excerpt("alpha beta gamma delta", 12)).toBe("alpha beta…");
  expect(excerpt("short", 12)).toBe("short");
});

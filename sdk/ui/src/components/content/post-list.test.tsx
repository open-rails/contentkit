// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { useMarkPostRead } from "../../react/post-reads.js";
import { PostList, excerpt, plainText } from "./post-list.js";

const later = new Date(Date.now() + 3_600_000).toISOString();
const card = (id: string, published_at: string, extra: Partial<Card> = {}): Card => ({
  id, kind: "blog", code: id, path: `/en/blog/${id}`, title: `Post ${id}`, language: "en", cover: [], published_at,
  version: { id, code: id, languages: ["en"], traits: [], has_audio: false }, entities: {}, counts: {}, access: { allowed: true, level: "public" }, attributes: {}, ...extra,
});
const texts: Record<string, { description?: string; body?: string }> = {
  fresh: { description: "A short summary." },
  old: { body: "<p>Hello <b>world</b>   with <i>markup</i></p>" },
  more: { description: "Second page." },
};
const model = { languages: ["en"], tiers: ["premium"], kinds: {}, entity_kinds: {} };
afterEach(() => { vi.unstubAllGlobals(); localStorage.clear(); });

function client(trending: Card[] = []) {
  const lists: URL[] = [];
  const c = createContentKitClient({ baseUrl: "https://content.test/blog", fetch: async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    if (url.pathname.endsWith("/media/presets")) return Response.json([]);
    const detail = /\/contents\/([^/]+)$/.exec(url.pathname);
    if (detail) return Response.json({ id: detail[1], kind: "blog", code: detail[1], path: "", language: "en", title: detail[1], versions: [], ...texts[detail[1] ?? ""] });
    lists.push(url);
    if (url.searchParams.get("sort")?.startsWith("popular")) return Response.json({ contents: trending });
    if (url.searchParams.get("cursor")) return Response.json({ contents: [card("more", "2020-01-01T00:00:00Z")] });
    const locked = card("old", "2020-01-01T00:00:00Z", { access: { allowed: false, level: "tier", tiers: ["premium"] } });
    return Response.json({ contents: [card("fresh", later), locked], next_cursor: url.searchParams.get("limit") === "2" ? "c2" : undefined });
  } });
  return { c, lists };
}

it("lists posts with excerpts, marks posts published since the first visit unread and adds a newest sidebar", async () => {
  const { c, lists } = client();
  render(<ContentKitProvider client={c} viewer={null}><PostList kind="blog" pageSize={2} tiers={["premium"]} /></ContentKitProvider>);
  expect(await screen.findByText("A short summary.")).toBeVisible();
  expect(await screen.findByText("Hello world with markup")).toBeVisible();
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
});

it("shows trending posts and forgets the unread mark once the post page marks it read", async () => {
  const { c } = client([card("hot", "2021-01-01T00:00:00Z", { title: "Hot post" })]);
  function Read({ id }: { id: string }) {
    useMarkPostRead(id);
    return null;
  }
  render(<ContentKitProvider client={c} viewer={null}><Read id="fresh" /><PostList kind="blog" /></ContentKitProvider>);
  const sidebar = await waitFor(() => document.querySelector('[data-ckui="post-list-sidebar"]') as HTMLElement);
  expect(await within(sidebar).findByRole("heading", { name: "Trending" })).toBeVisible();
  expect(within(sidebar).getByText("Hot post")).toBeVisible();
  await screen.findByText("A short summary.");
  const fresh = document.querySelector('[data-ckui="post-row"][data-id="fresh"]') as HTMLElement;
  expect(within(fresh).queryByText("Unread")).toBeNull();
});

it("trims markup and cuts excerpts at a word boundary", () => {
  expect(plainText("<p>One&nbsp;<em>two</em></p>\n<p>three</p>")).toBe("One two three");
  expect(excerpt("alpha beta gamma delta", 12)).toBe("alpha beta…");
  expect(excerpt("short", 12)).toBe("short");
});

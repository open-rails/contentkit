// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, ContentModel, GroupCard } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { GroupManager } from "../admin/group-manager.js";
import { GroupDirectory } from "../groups/directory.js";
import { ReleaseDateIndex } from "./filters.js";
import { ContentGrid } from "./grid.js";

const callers = ["grid", "dates", "directory", "manager"] as const;
type Caller = typeof callers[number];
const model: ContentModel = { languages: ["en"], tiers: [], signals: false, kinds: { gallery: { presentation: "reader", route: "g", versions: { languages: "one" } } }, entity_kinds: { artist: { route: "artists" } } };
const card = (id: string): Card => ({
  id, kind: "gallery", code: id, path: `/g/${id}`, title: `${id} item`, language: "en", cover: [],
  version: { id: `${id}-version`, code: id, languages: ["en"], traits: [], has_audio: false },
  entities: {}, counts: {}, attributes: {}, access: { allowed: true, level: "public" },
});
const group = (id: string): GroupCard => ({ id, kind: "artist", slug: id, name: `${id} item`, internal: false, count: 1, attributes: {}, created_at: "2026-10-16T00:00:00Z" });

function server(caller: Caller, failInitial: boolean) {
  const reads: URL[] = [];
  const target = caller === "grid" ? "/contents" : caller === "dates" ? "/release-dates" : caller === "directory" ? "/entities" : "/manage/entities";
  const client = createContentKitClient({ baseUrl: `https://retry.test/${caller}`, fetch: async (input, init) => {
    expect(init?.method).toBe("GET");
    const url = new URL(String(input));
    const path = url.pathname.slice(url.pathname.indexOf("/v1") + 3);
    if (path === "/kinds") return Response.json(model);
    if (path.endsWith("/members")) return Response.json({ code: "not_found" }, { status: 404 });
    if (path !== target) return Response.json({ code: "not_found" }, { status: 404 });
    reads.push(url);
    const cursor = url.searchParams.get("cursor");
    if ((failInitial && reads.length === 1) || (!failInitial && reads.length === 2)) return Response.json({ code: "unavailable", error: "Temporarily unavailable" }, { status: 503 });
    const ids = cursor ? ["First", "Second"] : ["First"];
    const next_cursor = cursor ? undefined : "next";
    return Response.json(caller === "grid" ? { contents: ids.map(card), next_cursor } : caller === "dates" ? {
      dates: ids.map((id) => ({ date: id === "First" ? "2026-10-16" : "2026-10-15", count: 1, free: 1 })), next_cursor,
    } : { entities: ids.map(group), next_cursor });
  } });
  return { client, reads };
}

function mount(caller: Caller, client: ReturnType<typeof server>["client"]) {
  return render(<ContentKitProvider client={client} viewer="staff">
    {caller === "grid" ? <ContentGrid query={{ kind: "gallery" }} renderCard={(card) => <span>{card.title}</span>} /> : caller === "dates" ? <ReleaseDateIndex query={{ kind: "gallery", by: "day" }} /> : caller === "directory" ? <GroupDirectory kind="artist" letters={false} /> : <GroupManager groupKinds={["artist"]} />}
  </ContentKitProvider>);
}

const first = (caller: Caller) => caller === "dates" ? screen.getByRole("button", { name: /Oct 16, 2026/ }) : screen.getByText("First item");
const second = (caller: Caller) => caller === "dates" ? screen.findByRole("button", { name: /Oct 15, 2026/ }) : screen.findByText("Second item");

it.each(callers)("%s retries a failed first page explicitly instead of showing an empty result", async (caller) => {
  const s = server(caller, true);
  mount(caller, s.client);
  expect(await screen.findByRole("alert")).toBeVisible();
  await act(async () => {});
  expect(s.reads).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await waitFor(() => expect(first(caller)).toBeVisible());
  expect(screen.queryByRole("alert")).toBeNull();
  expect(s.reads).toHaveLength(2);
  expect(s.reads.map((url) => url.searchParams.get("cursor"))).toEqual([null, null]);
});

it.each(callers)("%s exposes a failed next page and retries that cursor while retaining existing rows", async (caller) => {
  const s = server(caller, false);
  mount(caller, s.client);
  await waitFor(() => expect(first(caller)).toBeVisible());
  const original = first(caller);
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByRole("alert")).toBeVisible();
  expect(first(caller)).toBe(original);
  await act(async () => {});
  expect(s.reads).toHaveLength(2);
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await second(caller)).toBeVisible();
  expect(first(caller)).toBe(original);
  expect(screen.queryByRole("alert")).toBeNull();
  expect(s.reads.map((url) => url.searchParams.get("cursor"))).toEqual([null, "next", "next"]);
  expect(caller === "dates" ? screen.getAllByRole("button", { name: /Oct 16, 2026/ }) : screen.getAllByText("First item")).toHaveLength(1);
});

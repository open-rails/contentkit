// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { AdminComment, ContentModel, HeldItem } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { UserCommentStanding } from "../admin/user-comment-standing.js";
import { EntityMembers } from "../entities/entity-members.js";
import { CommentModeration } from "../social/moderation.js";

const callers = ["members", "standing", "queue"] as const;
type Caller = typeof callers[number];
const at = "2026-10-16T00:00:00Z";
const model: ContentModel = { languages: ["en"], tiers: [], signals: false, kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" } } }, entity_kinds: { artist: { route: "artists" } } };
const comment = (id: string): AdminComment => ({ id, body: `${id} item`, tenant_id: "site", content_kind: "gallery", content_id: "gallery", deleted: false, likes: 0, dislikes: 0, mine: 0, reply_count: 0, created_at: at, updated_at: at });
const held = (id: string): HeldItem => ({ id, kind: "comment", revision: 1, ref: { tenant_id: "site", content_kind: "gallery", content_id: "gallery" }, body: `${id} item`, reason: "review", held_at: at, created_at: at });

function server(caller: Caller, failInitial: boolean) {
  const reads: URL[] = [];
  const client = createContentKitClient({ baseUrl: `https://staff-retry.test/${caller}`, fetch: async (input, init) => {
    expect(init?.method).toBe("GET");
    const url = new URL(String(input));
    const path = url.pathname.slice(url.pathname.indexOf("/v1") + 3);
    if (path === "/kinds") return Response.json(model);
    // Actual staff probes still go through the client. Only the tested area is
    // allowed; distinguish its small probe from the actual author's/queue read.
    const list = caller === "members" ? path === "/entities/g1/members" : caller === "standing" ? path === "/comments/admin" && url.searchParams.get("author") === "author" : path === "/moderation/held" && url.searchParams.get("limit") === "2";
    if (!list) {
      if (caller === "standing" && path === "/comments/admin") return Response.json([]);
      if (caller === "queue" && path === "/moderation/held") return Response.json({ items: [] });
      return Response.json({ code: "forbidden" }, { status: 403 });
    }
    reads.push(url);
    if ((failInitial && reads.length === 1) || (!failInitial && reads.length === 2)) return Response.json({ code: "unavailable", error: "Temporarily unavailable" }, { status: 503 });
    const next = url.searchParams.has("cursor") || Number(url.searchParams.get("offset")) > 0;
    if (caller === "standing") return Response.json([comment(next ? "Second" : "First")]);
    if (caller === "queue") return Response.json({ items: [held(next ? "Second" : "First")], next: next ? undefined : "next" });
    return Response.json({ members: [{ content_id: next ? "Second" : "First" }], next_cursor: next ? undefined : "next" });
  } });
  return { client, reads };
}

function mount(caller: Caller, client: ReturnType<typeof server>["client"]) {
  return render(<ContentKitProvider client={client} viewer="staff">
    {caller === "members" ? <EntityMembers entity={{ id: "g1", kind: "artist", name: "Entity", slug: "group" }} renderContent={(member) => <span>{member.content_id} item</span>} /> : caller === "standing" ? <UserCommentStanding user="author" pageSize={1} /> : <CommentModeration sections={["queue"]} pageSize={2} />}
  </ContentKitProvider>);
}

it.each(callers)("%s preserves its initial error and retries the first page", async (caller) => {
  const s = server(caller, true);
  mount(caller, s.client);
  expect(await screen.findByRole("alert")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("First item")).toBeVisible();
  expect(screen.queryByRole("alert")).toBeNull();
  expect(s.reads).toHaveLength(2);
});

it.each(callers)("%s exposes a failed next page and retries without replacing its existing row", async (caller) => {
  const s = server(caller, false);
  mount(caller, s.client);
  const original = await screen.findByText("First item");
  fireEvent.click(screen.getByRole("button", { name: caller === "members" ? "Load more" : "Show more" }));
  expect(await screen.findByRole("alert")).toBeVisible();
  expect(screen.getByText("First item")).toBe(original);
  await act(async () => {});
  expect(s.reads).toHaveLength(2);
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Second item")).toBeVisible();
  expect(screen.getByText("First item")).toBe(original);
  expect(screen.queryByRole("alert")).toBeNull();
  await waitFor(() => expect(s.reads).toHaveLength(3));
  expect(s.reads.map((url) => caller === "standing" ? url.searchParams.get("offset") : url.searchParams.get("cursor"))).toEqual(caller === "standing" ? ["0", "1", "1"] : [null, "next", "next"]);
});

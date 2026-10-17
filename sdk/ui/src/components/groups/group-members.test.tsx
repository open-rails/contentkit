// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, Content, ContentModel } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { GroupMembers } from "./group-members.js";

const at = "2026-10-16T00:00:00Z";
const model: ContentModel = {
  languages: ["en"],
  tiers: ["premium"],
  signals: false,
  kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" } }, blog: { route: "blog", presentation: "post", versions: { languages: "one" } } },
  entity_kinds: { series: { route: "series" } },
};
const content = (id: string, title: string, kind: string, extra: Partial<Content> = {}): Content =>
  ({
    id, kind, code: id.toUpperCase(), path: `/en/g/${id}`, language: "en", title, state: "published", live: true,
    access: { level: "public" }, versions: [], cover: { url: `https://img.test/${id}.webp` }, created_at: at, updated_at: at, ...extra,
  }) as unknown as Content;
const stored: Record<string, Content> = {
  c1: content("c1", "Alpha", "gallery", { access: { level: "tier", tiers: ["premium"] } }),
  c2: content("c2", "Beta", "gallery", { state: "draft", live: false }),
  c3: content("c3", "Notes", "blog"),
};
const suggestion = { id: "c4", kind: "gallery", code: "C4", title: "Gamma", cover: [{ url: "https://img.test/c4.webp", w: 1, h: 1 }] } as unknown as Card;

function setup() {
  const added: unknown[] = [];
  const manageIds: string[][] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    const path = url.pathname.slice(url.pathname.indexOf("/v1") + 3);
    if (path === "/kinds") return Response.json(model);
    if (path === "/entities/g1/members" && init?.method === "POST") {
      added.push(JSON.parse(String(init.body)));
      return Response.json({ count: 1 });
    }
    if (path === "/entities/g1/members") return Response.json({ members: [{ content_id: "c1" }, { content_id: "c2" }, { content_id: "c3" }] });
    if (path === "/manage") {
      const ids = url.searchParams.getAll("ids").flatMap((x) => x.split(",")).filter(Boolean);
      manageIds.push(ids);
      return Response.json(ids.map((id) => stored[id]).filter(Boolean));
    }
    if (path === "/manage/search-suggestions") return Response.json({ contents: url.searchParams.get("q") ? [suggestion] : [], entities: [] });
    return Response.json({ code: "not_found" }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "https://ck.test", fetch: fetch as typeof globalThis.fetch });
  render(
    <ContentKitProvider client={client} viewer="staff">
      <GroupMembers group={{ id: "g1", kind: "series", name: "Saga", slug: "saga" }} contentHref={(c) => `/admin/galleries/${c.code}`} labels={{ gallery: "Galleries", blog: "Posts" }} />
    </ContentKitProvider>,
  );
  return { added, manageIds };
}

it("shows members as cover cards grouped by content kind, with access and state, hydrated in one managed read", async () => {
  const { manageIds } = setup();
  const alpha = await screen.findByRole("link", { name: "Alpha" });
  expect(alpha).toHaveAttribute("href", "/admin/galleries/C1");
  expect(manageIds).toEqual([["c1", "c2", "c3"]]);
  const galleries = screen.getByRole("heading", { name: "Galleries" }).closest("section")!;
  expect(within(galleries).getByText("Beta")).toBeVisible();
  expect(within(galleries).getByText("Draft")).toBeVisible();
  expect(within(galleries).getByText(/^premium$/i)).toBeVisible();
  const posts = screen.getByRole("heading", { name: "Posts" }).closest("section")!;
  expect(within(posts).getByText("Notes")).toBeVisible();
  expect(screen.queryByLabelText("Content id")).toBeNull();
});

it("adds a member by searching its title, without typing ids", async () => {
  const { added } = setup();
  await screen.findByText("Alpha");
  const user = userEvent.setup();
  await user.type(screen.getByRole("combobox", { name: "Search titles to add" }), "Gam");
  await user.click(await screen.findByRole("option", { name: /Gamma/ }));
  await waitFor(() => expect(added).toEqual([[{ content_id: "c4" }]]));
});

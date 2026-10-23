// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { EntityCard, EntityDetail } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { EntityBanner } from "./banner.js";
import { ScopedEntities } from "./scoped.js";

const model = { languages: ["en"], entity_kinds: { series: { route: "series" }, character: { route: "characters", scoped_by: "series" }, tag: { route: "tags" } } };
const card = (i: number): EntityCard => ({ id: `c${i}`, kind: "character", slug: `char-${i}`, internal: false, name: `Character ${i}`, path: `/en/characters/C${i}`, count: 20 - i, attributes: {}, created_at: "2026-10-01T00:00:00Z" }) as EntityCard;

function setup(characters: EntityCard[]) {
  const queries: URLSearchParams[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://x");
    if (url.pathname.endsWith("/kinds")) return Response.json(model);
    if (url.pathname.endsWith("/entities")) {
      queries.push(url.searchParams);
      return Response.json({ entities: url.searchParams.get("scope") === "s1" ? characters : [] });
    }
    return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  return { client, queries };
}

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer={null}>
        {children}
      </ContentKitProvider>
    );
  };

it("ScopedEntities: a series lists its characters most used first, linked, with Show more past the limit", async () => {
  const s = setup(Array.from({ length: 4 }, (_, i) => card(i + 1)));
  render(<ScopedEntities entityy={{ id: "s1", kind: "series" }} limit={3} />, { wrapper: wrap(s.client) });
  const section = await screen.findByRole("region", { name: "Characters" });
  const links = within(section).getAllByRole("link");
  expect(links.map((a) => a.getAttribute("href"))).toEqual(["/en/characters/C1", "/en/characters/C2", "/en/characters/C3"]);
  expect(s.queries.at(-1)?.get("kind")).toBe("character");
  expect(s.queries.at(-1)?.get("sort")).toBe("count");
  await userEvent.setup().click(within(section).getByRole("button", { name: "Show more" }));
  expect(within(section).getAllByRole("link")).toHaveLength(4);
  expect(within(section).queryByRole("button", { name: "Show more" })).toBeNull();
});

it("ScopedEntities: nothing for kinds nobody is scoped by, or a scope without members", async () => {
  const s = setup([]);
  const { container } = render(
    <>
      <ScopedEntities entityy={{ id: "t1", kind: "tag" }} />
      <ScopedEntities entityy={{ id: "s1", kind: "series" }} />
    </>,
    { wrapper: wrap(s.client) },
  );
  await vi.waitFor(() => expect(s.queries.length).toBeGreaterThan(0));
  expect(s.queries.every((q) => q.get("scope") === "s1")).toBe(true);
  expect(container.querySelector("[data-ckui='scoped-group-section']")).toBeNull();
});

it("EntityBanner: a character links its series and a series shows its characters", async () => {
  const s = setup([card(1)]);
  const base = { slug: "x", internal: false, state: "active", attributes: {}, names: [], text: [], children: [], edges: [], counts: [], created_at: "", updated_at: "", revision: 1 };
  const character = { ...base, id: "c1", kind: "character", name: "Character 1", scope_id: "s1", scope: { id: "s1", kind: "series", slug: "bleach", internal: false, name: "Bleach", path: "/en/series/S1", count: 3, attributes: {}, created_at: "" } } as unknown as EntityDetail;
  const { unmount } = render(<EntityBanner entityy={character} />, { wrapper: wrap(s.client) });
  expect(screen.getByRole("link", { name: "Bleach" })).toHaveAttribute("href", "/en/series/S1");
  unmount();
  render(<EntityBanner entityy={{ ...base, id: "s1", kind: "series", name: "Bleach" } as unknown as EntityDetail} />, { wrapper: wrap(s.client) });
  expect(await screen.findByRole("link", { name: /Character 1/ })).toHaveAttribute("href", "/en/characters/C1");
});

it("EntityBanner: a cover tile only for a group with a cover image", () => {
  const s = setup([]);
  const base = { id: "t1", kind: "tag", slug: "romance", name: "Romance", internal: false, state: "active", attributes: {}, names: [], text: [], children: [], edges: [], counts: [], created_at: "", updated_at: "", revision: 1 };
  const { container, unmount } = render(<EntityBanner entityy={base as unknown as EntityDetail} />, { wrapper: wrap(s.client) });
  expect(container.querySelector("[data-ckui=group-cover]")).toBeNull();
  expect(screen.getByRole("heading", { name: "Romance" })).toBeVisible();
  unmount();
  const covered = { ...base, cover: { renditions: [{ url: "https://cdn.test/romance.webp", w: 900, h: 300 }] } };
  const next = render(<EntityBanner entityy={covered as unknown as EntityDetail} />, { wrapper: wrap(s.client) });
  expect(next.container.querySelector("[data-ckui=group-cover]")).not.toBeNull();
});

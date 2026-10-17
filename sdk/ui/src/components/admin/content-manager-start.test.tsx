// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen } from "@testing-library/react";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { ContentModel } from "../../client/generated/wire.js";
import { ContentManager } from "./content-manager.js";

const model: ContentModel = {
  languages: ["en"],
  tiers: [],
  signals: false,
  kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" }, entities: ["series", "character"] } },
  entity_kinds: { series: { route: "series" }, character: { route: "characters", scoped_by: "series" } },
};
const at = "2026-10-16T00:00:00Z";
const card = (id: string, kind: string, name: string) => ({ id, kind, slug: name.toLowerCase(), name, internal: false, count: 0, attributes: {}, created_at: at });

it("opens a new item linked to the entity named by ?with=, with that entity's scope", async () => {
  const calls: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(String(input), "https://test.invalid").pathname.replace(/^\/v1/, "");
    calls.push(path);
    if (path === "/kinds") return Response.json(model);
    if (path === "/codes/MAI0000001") return Response.json({ code: "MAI0000001", content_kind: "character", content_id: "ch1", path: "/en/characters/MAI0000001/mai" });
    if (path === "/manage/entities/ch1") return Response.json({ ...card("ch1", "character", "Mai"), counts: [], scope_id: "s1", scope: card("s1", "series", "Saga") });
    return Response.json({ code: "not_found", error: `Unexpected ${path}` }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  function Route() {
    const [loc, setLoc] = useState({ pathname: "/galleries/new", search: "?with=MAI0000001" });
    return <ContentManager client={client} kind="gallery" basePath="/galleries" navigation={{ ...loc, navigate: (to) => setLoc({ pathname: to.split("?")[0]!, search: "" }) }} />;
  }
  render(<Route />);
  expect(await screen.findByText("Mai")).toBeVisible();
  expect(await screen.findByText("Saga")).toBeVisible();
  expect(calls).toContain("/manage/entities/ch1");
});

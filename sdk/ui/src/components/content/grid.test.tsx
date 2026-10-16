// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { Card } from "../../client/generated/wire.js";
import { beaconsFor, ContentKitProvider } from "../../react/index.js";
import { ContentGrid } from "./grid.js";

const card = (id: string, allowed = true): Card => ({
  id,
  kind: "gallery",
  code: id,
  path: `/en/g/${id}/${id}`,
  title: `Title ${id}`,
  language: "en",
  cover: [],
  version: { id: `${id}v`, code: "X1", languages: ["en"], traits: [], has_audio: id === "a" },
  entities: {},
  counts: {},
  access: allowed ? { allowed, level: "public" } : { allowed, level: "tier", tiers: ["premium"], sell: ["premium"] },
  attributes: {},
});

// A catalog of two open and two premium cards; each read is logged.
function serve() {
  const reads: URL[] = [];
  const client = createContentKitClient({
    baseUrl: "https://ck.test",
    fetch: async (input) => {
      const u = new URL(String(input));
      reads.push(u);
      const hide = u.searchParams.get("mode") === "hide";
      if (u.pathname === "/v1/release-dates") return Response.json({ dates: [{ date: "2026-10", count: hide ? 2 : 4, free: 2 }] });
      if (u.pathname === "/v1/contents")
        return Response.json({ contents: hide ? [card("a"), card("b")] : u.searchParams.has("access") ? [card("p1", false), card("p2", false)] : [] });
      return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
    },
  });
  return { client, reads };
}

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} navigate={vi.fn()}>
        {children}
      </ContentKitProvider>
    );
  };

it("decorates each cover with the host's badges, and a click still reports the card", async () => {
  const { client } = serve();
  const click = vi.spyOn(beaconsFor(client), "click");
  render(<ContentGrid query={{ kind: "gallery" }} locked="hide" decorate={(c) => c.version.has_audio && <i>audio</i>} />, { wrapper: wrap(client) });
  const first = (await screen.findByText("Title a")).closest("article")!;
  expect(within(first.querySelector('[data-ckui="card-decorations"]') as HTMLElement).getByText("audio")).toBeInTheDocument();
  expect(screen.getByText("Title b").closest("article")!.querySelector('[data-ckui="card-decorations"]')).toBeNull();
  fireEvent.click(screen.getByText("Title b"));
  expect(click).toHaveBeenCalledWith("b", expect.objectContaining({ position: 2, render_id: expect.any(String) }));
});

it("collapses locked cards behind a counted toggle that loads them when opened", async () => {
  const { client, reads } = serve();
  const click = vi.spyOn(beaconsFor(client), "click");
  render(<ContentGrid query={{ kind: "gallery", sort: "newest" }} locked="collapse" />, { wrapper: wrap(client) });
  await screen.findByText("Title a");
  const toggle = await screen.findByRole("button", { name: "Show 2 locked items" });
  expect(reads.filter((u) => u.pathname === "/v1/contents").map((u) => u.searchParams.get("mode"))).toEqual(["hide"]);
  expect(screen.queryByText("Title p1")).toBeNull();

  fireEvent.click(toggle);
  expect(await screen.findByText("Title p2")).toBeInTheDocument();
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  const locked = reads.find((u) => u.pathname === "/v1/contents" && u.searchParams.get("mode") === "lock")!;
  expect([locked.searchParams.get("access"), locked.searchParams.get("sort")]).toEqual(["tier,group,ppv,group_ppv", "newest"]);
  fireEvent.click(screen.getByText("Title p2"));
  fireEvent.click(screen.getByText("Title a"));
  const [p2, a] = click.mock.calls as [string, { position?: number; render_id?: string }][];
  expect([p2![0], p2![1].position, a![0], a![1].position]).toEqual(["p2", 2, "a", 1]);
  expect(p2![1].render_id).not.toBe(a![1].render_id);
  await waitFor(() => expect(screen.getByRole("button", { name: "Hide locked items" })).toBeInTheDocument());
});

// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel } from "../../client/generated/wire.js";
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

function blogServer() {
  const blogModel: ContentModel = { languages: ["en", "de"], tiers: [], signals: false, kinds: { blog: { route: "blog", presentation: "post", versions: { hidden: true, languages: "one" } } }, entity_kinds: {} };
  let post = { id: "p1", code: "POST1", kind: "blog", title: "Old title", state: "draft", live: false, language: "en", access: { level: "public" }, comments: "open", revision: 1, created_at: at, updated_at: at,
    text: { en: { title: "Old title", description: "", body: "" } }, versions: [] } as unknown as Content;
  const writes: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "https://test.invalid");
    const path = url.pathname.replace(/^\/v1/, "");
    if (path === "/kinds") return Response.json(blogModel);
    if (path === "/manage" || path === "/manage/blog") return Response.json([post]);
    if (path === "/contents/p1" && (init?.method ?? "GET") === "GET") return Response.json(post);
    if (path === "/contents/p1/text/en" && init?.method === "PUT") {
      // Like the server, the stored body is normalized.
      const text = { ...JSON.parse(String(init.body)), body: String(JSON.parse(String(init.body)).body ?? "").trim() };
      writes.push(`text:${text.title}`);
      post = { ...post, title: text.title, text: { en: text } } as Content;
      return Response.json(post);
    }
    if (path === "/contents/p1/publish") {
      writes.push(`publish:${(post.text as Record<string, { title: string }>).en!.title}`);
      return Response.json(post);
    }
    return Response.json({ code: "not_found", error: `Unexpected ${path}` }, { status: 404 });
  });
  const client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  render(<ContentManager client={client} kind="blog" basePath="/posts" languages={["en", "de"]} navigation={{ pathname: "/posts/POST1", search: "", navigate: vi.fn() }} />);
  return { writes };
}

it("saves a post's unsaved text before publishing it", async () => {
  const { writes } = blogServer();
  fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "New title" } });
  fireEvent.click(screen.getByRole("button", { name: "Publish" }));
  await waitFor(() => expect(writes).toEqual(["text:New title", "publish:New title"]));
});

it("asks before switching language discards a post's unsaved text", async () => {
  const user = userEvent.setup();
  blogServer();
  fireEvent.change(await screen.findByLabelText("Title"), { target: { value: "Unsaved" } });
  await user.click(screen.getByRole("combobox", { name: "Language" }));
  await user.click(await screen.findByRole("option", { name: "German" }));
  expect(await screen.findByText("Unsaved changes in this language will be discarded.")).toBeVisible();
  expect(screen.getByLabelText("Title")).toHaveValue("Unsaved");
});

it("takes the server's normalized text back after saving, so nothing reads as unsaved", async () => {
  const user = userEvent.setup();
  const { writes } = blogServer();
  fireEvent.change(await screen.findByLabelText("Body"), { target: { value: "Hello\n" } });
  const form = screen.getByLabelText("Body").closest("form")!;
  fireEvent.click(within(form).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(writes).toEqual(["text:Old title"]));
  await waitFor(() => expect(screen.getByLabelText("Body")).toHaveValue("Hello"));
  await user.click(screen.getByRole("combobox", { name: "Language" }));
  await user.click(await screen.findByRole("option", { name: "German" }));
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Language" })).toHaveTextContent("German"));
  expect(screen.queryByText("Unsaved changes in this language will be discarded.")).toBeNull();
});

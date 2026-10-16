// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { ContentModel } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { ContentAdmin } from "./content-admin.js";

const model: ContentModel = {
  languages: ["en", "ja"], tiers: [], signals: true, content_filters: [],
  kinds: {
    article: { route: "articles", presentation: "post", versions: { languages: "one" } },
    video: { route: "videos", presentation: "video", versions: { languages: "one" } },
  },
  entity_kinds: { artist: { route: "artists" }, board: { owner: "user" } },
};
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const denied = () => json({ code: "forbidden", error: "Forbidden" }, 403);

function server(kinds = model, reply?: (url: URL) => Response | undefined) {
  const requests: URL[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    expect(init?.method ?? "GET").toBe("GET");
    const url = new URL(String(input), "https://test.invalid");
    requests.push(url);
    const answer = reply?.(url);
    if (answer) return answer;
    const path = url.pathname.replace(/^\/v1/, "");
    if (path === "/kinds") return json(kinds);
    if (path === "/manage" || path.startsWith("/manage/") || path === "/polls/admin") return json([]);
    if (path === "/stats/activity") return json({ days: [] });
    if (path === "/stats/top") return json({ contents: [] });
    if (path === "/entities") return json({ entities: [] });
    if (path.startsWith("/entities/") && path.endsWith("/members")) return json({ code: "not_found", error: "Not found" }, 404);
    if (path === "/moderation/banned-words") return json([]);
    return denied();
  });
  return { requests, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}

const nav = () => within(screen.getByRole("navigation", { name: "Administration sections" }));
const paths = (s: ReturnType<typeof server>) => s.requests.map((r) => r.pathname.replace(/^\/v1/, ""));
afterEach(() => window.history.replaceState(null, "", "/"));

it("discovers kinds and languages, respects configured order and mounts only the selected manager", async () => {
  const s = server();
  const user = userEvent.setup();
  render(<ContentAdmin client={s.client} sections={["contents", "polls", "statistics"]} contentKinds={["video", "missing", "article"]} labels={{ article: "Articles", video: "Videos" }} />);
  const picker = await screen.findByRole("combobox", { name: "Content kind" });
  expect(within(picker).getAllByRole("option").map((x) => x.textContent)).toEqual(["Videos", "Articles"]);
  expect(picker).toHaveValue("video");
  await screen.findByText("Nothing matches.");
  expect(s.requests.some((r) => r.pathname.endsWith("/manage/video") && r.searchParams.get("language") === "en")).toBe(true);
  expect(paths(s).every((p) => p === "/kinds" || p.startsWith("/manage"))).toBe(true);
  await user.selectOptions(picker, "article");
  await waitFor(() => expect(s.requests.some((r) => r.pathname.endsWith("/manage/article"))).toBe(true));
  await user.click(nav().getByRole("button", { name: "Polls" }));
  await screen.findByText("No polls match.");
  expect(screen.queryByRole("combobox", { name: "Content kind" })).not.toBeInTheDocument();
  expect(paths(s)).toContain("/polls/admin");
  expect(paths(s).some((p) => p.startsWith("/stats"))).toBe(false);
});

it("keeps deep links, child navigation and browser Back in the host path", async () => {
  window.history.replaceState(null, "", "/work/contents/video?status=draft");
  const s = server();
  const user = userEvent.setup();
  render(<ContentAdmin client={s.client} basePath="/work" sections={["contents", "polls"]} />);
  expect(await screen.findByRole("combobox", { name: "Content kind" })).toHaveValue("video");
  await waitFor(() => expect(s.requests.some((r) => r.pathname.endsWith("/manage/video") && r.searchParams.get("state") === "draft")).toBe(true));
  await user.click(await screen.findByRole("button", { name: "New item" }));
  await waitFor(() => expect(window.location.pathname).toBe("/work/contents/video/new"));
  expect(await screen.findByRole("button", { name: "Create draft" })).toBeVisible();
  await user.click(nav().getByRole("button", { name: "Polls" }));
  expect(window.location.pathname).toBe("/work/polls");
  window.history.back();
  await waitFor(() => expect(window.location.pathname).toBe("/work/contents/video/new"));
  expect(await screen.findByRole("button", { name: "Create draft" })).toBeVisible();
});

it.each([
  ["/work/contents/missing", ["contents"] as const],
  ["/work/contents/article", ["contents"] as const],
  ["/work/groups/board", ["groups"] as const],
  ["/work/polls", ["contents"] as const],
  ["/work/contents/%2F", ["contents"] as const],
])("does not load a manager for an unknown or excluded route %s", async (pathname, sections) => {
  const s = server();
  render(<ContentAdmin client={s.client} basePath="/work" navigation={{ pathname, search: "", navigate: vi.fn() }} sections={sections} contentKinds={["video"]} />);
  expect(await screen.findByText("This view is not available in this workbench.")).toBeVisible();
  expect(paths(s)).toEqual(["/kinds"]);
});

it("uses native manager refusal and keeps statistics independent of content-write access", async () => {
  const s = server(model, (url) => url.pathname.includes("/manage") ? denied() : undefined);
  const user = userEvent.setup();
  render(<ContentAdmin client={s.client} sections={["contents", "statistics"]} contentKinds={["video"]} />);
  expect(await screen.findByText("You don't have permission to manage this content.")).toBeVisible();
  await user.click(nav().getByRole("button", { name: "Statistics" }));
  expect(await screen.findByRole("heading", { name: "Views per day" })).toBeVisible();
  await waitFor(() => expect(paths(s)).toContain("/stats/activity"));
  expect(s.requests.filter((r) => r.pathname.includes("/stats/")).every((r) => r.searchParams.get("kind") === "video")).toBe(true);
});

it("drops a previous viewer's rows and rechecks permissions when the account scope changes", async () => {
  let refuse = false;
  const s = server(model, (url) => {
    if (!url.pathname.includes("/manage")) return undefined;
    return refuse ? denied() : json([{ id: "draft-one", title: "First account draft", kind: "article", state: "draft", live: false, language: "en", updated_at: "2026-10-01T00:00:00Z" }]);
  });
  const body = (viewer: string) => <ContentKitProvider client={s.client} viewer={viewer}><ContentAdmin sections={["contents"]} contentKinds={["article"]} /></ContentKitProvider>;
  const view = render(body("first"));
  expect(await screen.findByRole("button", { name: "First account draft" })).toBeVisible();
  refuse = true;
  view.rerender(body("second"));
  expect(await screen.findByText("You don't have permission to manage this content.")).toBeVisible();
  expect(screen.queryByText("First account draft")).not.toBeInTheDocument();
});

it("adopts a different client's model without keeping the previous app's kind", async () => {
  const first = server({ ...model, kinds: { article: model.kinds.article! } });
  const second = server({ ...model, kinds: { video: model.kinds.video! } });
  const view = render(<ContentAdmin client={first.client} sections={["contents"]} />);
  expect(await screen.findByRole("combobox", { name: "Content kind" })).toHaveValue("article");
  view.rerender(<ContentAdmin client={second.client} sections={["contents"]} />);
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Content kind" })).toHaveValue("video"));
  expect(screen.queryByRole("option", { name: "Article" })).not.toBeInTheDocument();
  expect(second.requests.filter((r) => r.pathname.includes("/manage/")).every((r) => r.pathname.endsWith("/manage/video"))).toBe(true);
});

it("retries model discovery and makes an empty configured scope explicit", async () => {
  let failed = true;
  const s = server(model, (url) => url.pathname.endsWith("/kinds") && failed ? json({ code: "unavailable", error: "Unavailable" }, 503) : undefined);
  const user = userEvent.setup();
  render(<ContentAdmin client={s.client} sections={["contents"]} contentKinds={["missing"]} />);
  await screen.findByRole("button", { name: "Try again" });
  failed = false;
  await user.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("No administration sections are configured.")).toBeVisible();
  expect(paths(s).every((p) => p === "/kinds")).toBe(true);
});

it("composes the native group and moderation managers without loading other sections", async () => {
  const s = server();
  const user = userEvent.setup();
  render(<ContentAdmin client={s.client} sections={["groups", "moderation"]} labels={{ artist: "Artists" }} moderation={{ sections: ["words"] }} />);
  await waitFor(() => expect(s.requests.some((r) => r.pathname.endsWith("/entities") && r.searchParams.get("kind") === "artist")).toBe(true));
  expect(s.requests.some((r) => r.searchParams.get("kind") === "board")).toBe(false);
  await user.click(nav().getByRole("button", { name: "Comment moderation" }));
  expect(await screen.findByLabelText("Words to ban")).toBeVisible();
  expect(paths(s)).toContain("/moderation/banned-words");
  expect(paths(s).some((p) => p.startsWith("/manage") || p === "/polls/admin" || p.startsWith("/stats/"))).toBe(false);
});

it("passes nested routes and filters through the host navigation adapter", async () => {
  const s = server();
  const navigate = vi.fn();
  render(<ContentAdmin client={s.client} basePath="/host/content" navigation={{ pathname: "/host/content/contents/article", search: "?q=needle", navigate }} sections={["contents"]} />);
  await waitFor(() => expect(s.requests.some((r) => r.pathname.endsWith("/manage/article") && r.searchParams.get("q") === "needle")).toBe(true));
  fireEvent.click(await screen.findByRole("button", { name: "New item" }));
  expect(navigate).toHaveBeenCalledWith("/host/content/contents/article/new", {});
});

it.each(["viewer", "client"] as const)("discards an unsaved editor when the %s changes", async (changed) => {
  window.history.replaceState(null, "", "/work/contents/article/new");
  const a = server();
  const b = server();
  const body = (client = a.client, viewer = "first") => <ContentKitProvider client={client} viewer={viewer}><ContentAdmin basePath="/work" sections={["contents"]} /></ContentKitProvider>;
  const view = render(body());
  const title = await screen.findByLabelText("Title");
  fireEvent.change(title, { target: { value: "Private unfinished title" } });
  expect(title).toHaveValue("Private unfinished title");
  view.rerender(changed === "viewer" ? body(a.client, "second") : body(b.client));
  await waitFor(() => expect(screen.getByLabelText("Title")).toHaveValue(""));
  expect(window.location.pathname).toBe("/work/contents/article/new");
});

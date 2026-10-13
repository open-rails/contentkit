// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { BannedWord, IPBan, ReaderMeta, ReaderPlan } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { CommentModeration, ReaderManifestEditor } from "../../index.js";

type Handler = (req: { url: URL; body: unknown }) => unknown;

/** A fetch answering "METHOD /path" from routes; calls lists what was asked, bodies what was sent. */
function server(routes: Record<string, Handler>) {
  const calls: string[] = [];
  const bodies: Record<string, unknown[]> = {};
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://x");
    const key = `${init?.method ?? "GET"} ${url.pathname.replace(/^\/v1/, "")}`;
    calls.push(key);
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (body !== undefined) (bodies[key] ??= []).push(body);
    const h = routes[key];
    if (!h) return json(404, { error: "not found", code: "not_found" });
    const out = await h({ url, body });
    return out instanceof Response ? out : out === undefined ? new Response(null, { status: 204 }) : json(200, out);
  });
  return { calls, bodies, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function wrap(client: ContentKitClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer="moderator">
        {children}
      </ContentKitProvider>
    );
  };
}

const at = "2026-10-01T10:00:00Z";

it("CommentModeration: bans words several at once, switches one to refusing, unbans; bans and lifts an IP network", async () => {
  let words: BannedWord[] = [{ word: "darn", action: "mask", created_by: "mod", created_at: at }];
  let ips: IPBan[] = [];
  const s = server({
    "GET /moderation/banned-words": () => words,
    "POST /moderation/banned-words": ({ body }) => {
      for (const w of body as { word: string; action: "mask" | "reject" }[]) words = [...words.filter((x) => x.word !== w.word), { word: w.word, action: w.action, created_by: "mod", created_at: at }];
      return words;
    },
    "DELETE /moderation/banned-words": ({ body }) => void (words = words.filter((w) => !(body as string[]).includes(w.word))),
    "GET /moderation/ip-bans": () => ips,
    "POST /moderation/ip-bans": ({ body }) => {
      const [b] = body as { network: string; reason?: string }[];
      ips = [{ network: b!.network, reason: b!.reason, banned_by: "mod", banned_at: at, expired: false }];
      return ips;
    },
    "DELETE /moderation/ip-bans": () => void (ips = []),
  });
  const user = userEvent.setup();
  render(<CommentModeration sections={["words", "ips"]} />, { wrapper: wrap(s.client) });
  const row = (await screen.findByText("darn")).closest("li")!;
  expect(within(row).getByText("Masks")).toBeInTheDocument();

  await user.type(screen.getByLabelText("Words to ban"), "heck, gosh,heck");
  await user.click(screen.getByRole("button", { name: "Ban" }));
  expect(await screen.findByText("gosh")).toBeInTheDocument();
  expect(s.bodies["POST /moderation/banned-words"]![0]).toEqual([
    { word: "heck", action: "mask" },
    { word: "gosh", action: "mask" },
  ]);
  expect(screen.getByLabelText("Words to ban")).toHaveValue("");

  await user.click(screen.getByRole("button", { name: "Refuse comments with “darn”" }));
  await waitFor(() => expect(within(screen.getByText("darn").closest("li")!).getByText("Refuses")).toBeInTheDocument());
  expect(s.bodies["POST /moderation/banned-words"]![1]).toEqual([{ word: "darn", action: "reject" }]);

  await user.click(screen.getByRole("button", { name: "Unban “gosh”" }));
  await waitFor(() => expect(screen.queryByText("gosh")).not.toBeInTheDocument());
  expect(s.bodies["DELETE /moderation/banned-words"]).toEqual([["gosh"]]);

  await user.click(screen.getByRole("tab", { name: "IP bans" }));
  expect(await screen.findByText("No IP bans.")).toBeInTheDocument();
  await user.type(screen.getByLabelText("Address or network"), "203.0.113.0/24");
  await user.type(screen.getByLabelText("Reason (optional)"), "spam wave");
  await user.click(screen.getByRole("button", { name: "Ban" }));
  const ban = (await screen.findByText("203.0.113.0/24")).closest("li")!;
  expect(within(ban).getByText("Reason: spam wave")).toBeInTheDocument();
  expect(s.bodies["POST /moderation/ip-bans"]).toEqual([[{ network: "203.0.113.0/24", reason: "spam wave" }]]);
  await user.click(within(ban).getByRole("button", { name: "Lift the ban on 203.0.113.0/24" }));
  expect(await screen.findByText("No IP bans.")).toBeInTheDocument();
  expect(s.bodies["DELETE /moderation/ip-bans"]).toEqual([["203.0.113.0/24"]]);
});

it("CommentModeration: without root:comments:moderate the rule sections say so", async () => {
  const refuse = () => json(403, { error: "forbidden", code: "forbidden" });
  const s = server({ "GET /moderation/banned-words": refuse, "GET /moderation/ip-bans": refuse });
  render(<CommentModeration sections={["words", "ips"]} />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("You don't have permission to moderate comments.")).toBeInTheDocument();
});

const page = (name: string) => ({ name, w: 800, h: 1200, files: { low: `low/${name}.webp` } });
const plan = (reader?: ReaderMeta): ReaderPlan => ({
  content_id: "c1",
  version_id: "v1",
  publication_id: "p1",
  pages: 3,
  segments: [{ start: 0, end: 3, released: true, locked: false, pages: [page("001"), page("002"), page("003")] }],
  previews: [],
  playback: { item: { kind: "gallery.published", id: "pb" }, tracks: ["001", "003", "theme"].map((name) => ({ name, path: `listen/${name}/` })) },
  reader,
});

it("ReaderManifestEditor: matches audio to pages, adds an anchor, refuses a bad offset, saves the manifest and reads the plan again", async () => {
  let stored: ReaderMeta = { schema_version: 1, anchors: [{ id: "a1", page: "002", offset_percent: 50 }], background: [{ file: "theme", loop: true }], foreground: [] };
  const s = server({
    "GET /contents/c1/versions/v1/reader": () => plan(stored),
    "PUT /contents/c1/versions/v1/reader": ({ body }) => void (stored = body as ReaderMeta),
  });
  const user = userEvent.setup();
  render(<ReaderManifestEditor content="c1" version="v1" />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("3 pages and 3 audio files, in reading order.")).toBeInTheDocument();
  expect(screen.getByLabelText("Anchor 1 id")).toHaveValue("a1");
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

  await user.click(screen.getByRole("button", { name: "Match audio to pages" }));
  expect(screen.getByText("Matched 2 audio files to pages or anchors.")).toBeInTheDocument();
  expect(screen.getByText("Pages without audio: 002")).toBeInTheDocument();
  expect(screen.getByText("Audio matching no page or anchor: theme")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Add anchor" }));
  expect(screen.getByLabelText("Anchor 2 id")).toHaveValue("a2");
  fireEvent.change(screen.getByLabelText("Anchor 2 offset (%)"), { target: { value: "150" } });
  expect(await screen.findByText("Anchor 2: the offset is 0 to 100.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Anchor 2 offset (%)"), { target: { value: "25" } });
  fireEvent.change(screen.getByLabelText("Auto-advance (seconds, optional)"), { target: { value: "6" } });
  await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());

  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Manifest saved.")).toBeInTheDocument();
  expect(s.bodies["PUT /contents/c1/versions/v1/reader"]).toEqual([
    {
      schema_version: 1,
      defaults: { auto_advance_seconds: 6 },
      anchors: [
        { id: "a1", page: "002", offset_percent: 50 },
        { id: "a2", page: "001", offset_percent: 25 },
      ],
      background: [{ file: "theme", loop: true }],
      foreground: [
        { file: "001", page: "001" },
        { file: "003", page: "003" },
      ],
    },
  ]);
  await waitFor(() => expect(s.calls.filter((c) => c === "GET /contents/c1/versions/v1/reader")).toHaveLength(2));
  expect(screen.getByLabelText("Anchor 2 offset (%)")).toHaveValue(25);
});

it("ReaderManifestEditor: a version still being published waits for it", async () => {
  const s = server({ "GET /contents/c1/versions/v1/reader": () => json(202, { status: "pending" }) });
  render(<ReaderManifestEditor content="c1" version="v1" />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("This version is being published; its manifest can be edited once it is.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
});

// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { BannedWord, FileInfo, IPBan, ReaderMeta, ReadResult } from "../../client/generated/wire.js";
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
  const s = server(Object.fromEntries(["/comments/admin", "/moderation/held", "/global-comment-bans", "/moderation/banned-words", "/moderation/ip-bans"].map((p) => [`GET ${p}`, refuse])));
  render(<CommentModeration sections={["words", "ips"]} />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("You don't have permission to moderate comments.")).toBeInTheDocument();
});

const hash = (name: string) => name.padStart(64, "0");
/** An editor read of an unpublished version: three pages with thumbnails, three audio uploads (theme converted) and the stored manifest. */
const read = (reader?: ReaderMeta): ReadResult => {
  const files: FileInfo[] = [
    ...["001", "002", "003"].flatMap((name): FileInfo[] => [
      { path: `pages/${name}.png`, type: "image/png", upload: true, page_id: hash(name) },
      { path: `thumb/${name}.webp`, type: "image/webp", from: `pages/${name}.png`, url: `https://test.invalid/thumb/${name}.webp`, w: 100, h: 150 },
    ]),
    ...["001", "003", "theme"].map((name): FileInfo => ({ path: `audio/${name}.mp3`, type: "audio/mpeg", upload: true })),
    { path: "listen/theme/audio.m4a", type: "audio/mp4", from: "audio/theme.mp3", url: "https://test.invalid/theme.m4a" },
  ];
  return {
    files, total: files.length, offset: 0, limit: 200, expires: 0, access: "full", meta: reader ? { reader } : undefined,
    uploads: [{ path: "pages/{name}", types: ["image/png"], max_bytes: 1000, page: true }, { path: "audio/{name}", types: ["audio/mpeg"], max_bytes: 1000 }],
  };
};

it("ReaderManifestEditor: matches audio to pages, adds an anchor, refuses a bad offset, saves the manifest and reads the files again", async () => {
  let stored: ReaderMeta = { schema_version: 1, anchors: [{ id: "a1", page: hash("002"), offset_percent: 50 }], background: [{ file: "theme", loop: true }], foreground: [] };
  const s = server({ "PUT /contents/c1/versions/v1/reader": ({ body }) => void (stored = body as ReaderMeta) });
  const media = vi.spyOn(s.client.media, "read").mockImplementation(async () => read(stored));
  const user = userEvent.setup();
  render(<ReaderManifestEditor kind="gallery" content="c1" version="v1" />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("3 pages and 3 audio files, in reading order.")).toBeInTheDocument();
  expect(media).toHaveBeenCalledWith({ kind: "gallery", id: "c1", version: "v1" }, expect.objectContaining({ editor: true }));
  expect(s.calls).not.toContain("GET /contents/c1/versions/v1/reader");
  expect(screen.getByLabelText("Anchor 1 id")).toHaveValue("a1");
  expect(screen.getByRole("button", { name: "Anchor 1 page" }).querySelector("img")).toHaveAttribute("src", "https://test.invalid/thumb/002.webp");
  expect(screen.getByRole("button", { name: "Play theme" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

  await user.click(screen.getByRole("button", { name: "Match audio to pages" }));
  expect(screen.getByText("Matched 2 audio files to pages or anchors.")).toBeInTheDocument();
  expect(screen.getByText("Pages without audio: 002.png")).toBeInTheDocument();
  expect(screen.getByText("Audio matching no page or anchor: theme")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Add anchor" }));
  expect(screen.getByLabelText("Anchor 2 id")).toHaveValue("a2");
  fireEvent.change(screen.getByLabelText("Anchor 2 offset (%)"), { target: { value: "150" } });
  expect(await screen.findByText("Anchor 2: the offset is 0 to 100.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Anchor 2 offset (%)"), { target: { value: "25" } });
  fireEvent.change(screen.getByLabelText("Auto-advance (seconds, optional)"), { target: { value: "6" } });
  await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());

  const reads = media.mock.calls.length;
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Manifest saved.")).toBeInTheDocument();
  expect(s.bodies["PUT /contents/c1/versions/v1/reader"]).toEqual([
    {
      schema_version: 1,
      defaults: { auto_advance_seconds: 6 },
      anchors: [
        { id: "a1", page: hash("002"), offset_percent: 50 },
        { id: "a2", page: hash("001"), offset_percent: 25 },
      ],
      background: [{ file: "theme", loop: true }],
      foreground: [
        { file: "001", page: hash("001") },
        { file: "003", page: hash("003") },
      ],
    },
  ]);
  await waitFor(() => expect(media.mock.calls.length).toBeGreaterThan(reads));
  expect(screen.getByLabelText("Anchor 2 offset (%)")).toHaveValue(25);
});

it("ReaderManifestEditor: seeds every stored cue and flags one naming audio the version no longer has", async () => {
  const stored: ReaderMeta = { schema_version: 1, anchors: [], background: [{ file: "gone" }], foreground: [{ file: "003", page: hash("003") }] };
  const s = server({ "PUT /contents/c1/versions/v1/reader": () => undefined });
  vi.spyOn(s.client.media, "read").mockImplementation(async () => read(stored));
  const user = userEvent.setup();
  render(<ReaderManifestEditor kind="gallery" content="c1" version="v1" />, { wrapper: wrap(s.client) });
  expect(await screen.findByRole("combobox", { name: "Cue 1 audio" })).toHaveTextContent("003");
  expect(screen.getByText("Track 1: names audio this version doesn't have.")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Remove track 1" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Manifest saved.")).toBeInTheDocument();
  expect(s.bodies["PUT /contents/c1/versions/v1/reader"]).toEqual([{ schema_version: 1, anchors: [], background: [], foreground: [{ file: "003", page: hash("003") }] }]);
});

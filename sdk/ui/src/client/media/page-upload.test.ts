import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../client.js";
import type { CommitBody, FileInfo, PresignBody, UploadRule } from "../generated/wire.js";
import type { UploadState } from "./client.js";
import { ItemUploadQueue } from "./queue.js";
import { sortPageFiles, uniquePageName } from "./rules.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "0192f000-0000-7000-8000-000000000002" };
const pages: UploadRule = { path: "pages/{name}", types: ["image/png"], max_bytes: 1000, page: true };
const file = (name: string) => new File([name], name, { type: "image/png" });

function server(options: { process?: boolean; multipart?: boolean; allocatedPath?: string } = {}) {
  const commits: CommitBody[] = [];
  let size = 0;
  const client = createContentKitClient({
    baseUrl: "https://api.test",
    media: { transport: async () => {}, retries: 0 },
    fetch: (async (input, init) => {
      const path = new URL(String(input)).pathname;
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      if (path.endsWith("/presign")) {
        const request = body as PresignBody;
        size = request.size;
        return Response.json({ path: request.path.replace(/\/(\d+)\./, (_, n: string) => `/${n.padStart(3, "0")}.`), source_name: request.path, blob: `upload-${request.sha256}`, process_on_upload: options.process,
          ...(options.multipart ? { multipart: { ticket: "ticket", min_part_size: 1000, max_part_size: 1000, max_parts: 2 } } : { put: { url: "https://storage.test/file", headers: {} } }) });
      }
      if (path.endsWith("/parts/list")) return Response.json({ code: "not_found", error: "Already completed" }, { status: 404 });
      if (path.endsWith("/parts")) return Response.json({ parts: body.parts.map((part: { number: number }) => ({ number: part.number, url: "https://storage.test/part", headers: {} })) });
      if (path.endsWith("/complete")) return Response.json({ blob: "upload", type: "image/png", size });
      if (path.endsWith("/commit")) {
        commits.push(body);
        return Response.json({ files: body.ops.map((op: { op: string; path: string; create_id?: string; unattached?: boolean; replace?: boolean }): FileInfo => ({ path: op.op === "put" && !op.replace ? options.allocatedPath ?? op.path : op.path, create_id: op.create_id, type: "image/png", upload: true, unattached: op.unattached, size: 4 })) });
      }
      return Response.json({ access: "full", expires: 0, total: 0, offset: 0, limit: 100, files: [] });
    }) as typeof fetch,
  });
  return { client, commits };
}

it("keeps the real original name through canonical presign and direct put", async () => {
  const s = server();
  await s.client.media.put(file("scan17.png"), { ref, path: "pages/17.png", wait: false });
  expect(s.commits[0]!.ops[0]).toMatchObject({ path: "pages/017.png", source_name: "scan17.png" });
  const unnamed = await s.client.media.upload(new Blob(["image"], { type: "image/png" }), { ref, path: "pages/18.png" });
  expect(unnamed).toMatchObject({ path: "pages/018.png", sourceName: "pages/18.png" });
});

it("explicitly replaces the requested page slot even when the new original filename differs", async () => {
  const s = server({ allocatedPath: "pages/017_2.png" });
  const stored = await s.client.media.put(file("replacement.png"), { ref, path: "pages/017.png", wait: false });
  expect(s.commits[0]!.ops[0]).toMatchObject({ path: "pages/017.png", source_name: "replacement.png", replace: true });
  expect(s.commits[0]!.ops[0]!.create_id).toBeUndefined();
  expect(stored.path).toBe("pages/017.png");
});

it("retains the source name in multipart state and when resuming a completed ticket", async () => {
  const s = server({ multipart: true });
  let saved: UploadState | undefined;
  const original = file("original 21.png");
  const uploaded = await s.client.media.upload(original, { ref, path: "pages/21.png", onState: (state) => { if (state) saved = state; } });
  expect(saved?.sourceName).toBe("original 21.png");
  expect(uploaded.sourceName).toBe("original 21.png");
  const resumed = await s.client.media.upload(original, { ref, path: "pages/21.png", resume: saved });
  expect(resumed.sourceName).toBe("original 21.png");
  const priorState = await s.client.media.upload(original, { ref, path: "pages/21.png", resume: { ...saved!, sourceName: undefined } });
  expect(priorState.sourceName).toBe("original 21.png");
});

it("carries source names through process-on-upload staging and final attachment", async () => {
  const s = server({ process: true });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await queue.commit();
    const put = s.commits.flatMap((commit) => commit.ops).find((op) => op.op === "put");
    expect(put).toMatchObject({ path: "pages/001.png", source_name: "1.png", unattached: true });
    expect(s.commits.at(-1)!.ops).toEqual([{ op: "attach", path: "pages/001.png", index: 2 ** 31 - 1 }]);
  } finally { queue.dispose(); }
});

it("appends a naturally sorted new batch without reordering the manually arranged queue", async () => {
  const s = server();
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}" });
  const batch = (names: string[]) => sortPageFiles(names.map((name) => ({ file: file(name), rule: pages }))).map((entry) => entry.file);
  try {
    const first = queue.add(batch(["3.png", "1.png"]));
    queue.move(first[1]!.id, 0);
    queue.add(batch(["10.png", "2.png"]));
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await queue.commit();
    expect(s.commits[0]!.ops.map((op) => op.source_name)).toEqual(["3.png", "1.png", "2.png", "10.png"]);
    expect(s.commits[0]!.ops.every((op) => op.index === 2 ** 31 - 1)).toBe(true);
    expect(s.commits[0]!.ops.every((op) => op.replace === undefined)).toBe(true);
  } finally { queue.dispose(); }
});

it("keeps distinct 1/001 source bytes in separate canonical upload slots", async () => {
  const s = server();
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}" });
  const taken: string[] = [];
  const paths = new Map<File, string>();
  for (const { file: source } of sortPageFiles([file("1.png"), file("001.png")].map((source) => ({ file: source, rule: pages })))) {
    const name = uniquePageName(source.name, taken);
    taken.push(name);
    paths.set(source, `pages/${name}`);
  }
  try {
    queue.add(paths.keys(), { path: (source) => paths.get(source)! });
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await queue.commit();
    const puts = s.commits[0]!.ops;
    expect(puts.map((op) => op.path)).toEqual(["pages/001.png", "pages/1 (2).png"]);
    expect(puts.map((op) => op.source_name)).toEqual(["001.png", "1.png"]);
    expect(new Set(puts.map((op) => op.blob)).size).toBe(2);
  } finally { queue.dispose(); }
});

it("finds a newly allocated path by create_id for direct puts and staged queue attachments", async () => {
  const direct = server({ allocatedPath: "pages/001_2.png" });
  expect((await direct.client.media.put(file("1.png"), { ref, path: "pages/1.png", createOnly: true, wait: false })).path).toBe("pages/001_2.png");
  expect(direct.commits[0]!.ops[0]!.create_id).toBeTruthy();
  expect(direct.commits[0]!.ops[0]!.replace).toBeUndefined();
  const appended = new ItemUploadQueue(direct.client.media, { ref, path: "pages/{name}" });
  try {
    appended.add([file("1.png")]);
    await vi.waitFor(() => expect(appended.getSnapshot().ready).toBe(true));
    await appended.commit();
    expect(appended.getSnapshot().items[0]?.path).toBe("pages/001_2.png");
    expect(appended.getSnapshot().items[0]?.result?.path).toBe("pages/001_2.png");
  } finally { appended.dispose(); }
  const staged = server({ process: true, allocatedPath: "pages/001_2.png" });
  const queue = new ItemUploadQueue(staged.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.unattached).toBe(true));
    expect(queue.getSnapshot().items[0]?.path).toBe("pages/001_2.png");
    await queue.commit();
    expect(staged.commits.at(-1)!.ops).toEqual([{ op: "attach", path: "pages/001_2.png", index: 2 ** 31 - 1 }]);
    expect(queue.getSnapshot().items[0]?.path).toBe("pages/001_2.png");
  } finally { queue.dispose(); }
});

it("leaves a stray the queue now holds to its queued attach", async () => {
  const s = server({ process: true, allocatedPath: "pages/002.png" });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.unattached).toBe(true));
    await queue.commit(undefined, { head: true, attach: ["pages/002.png", "pages/009.png"] });
    expect(s.commits.at(-1)!.ops.map((op) => `${op.op} ${op.path}`)).toEqual(["attach pages/002.png", "attach pages/009.png"]);
  } finally { queue.dispose(); }
});

/**
 * Pages as ContentKit stores them: presign offers every new page the first
 * slot, a staging put takes the next number, a put of a known create ID is a
 * retry and does nothing, and an attach moves an unattached page to the end.
 */
function stagingServer(o: { presign?: (path: string) => Promise<void> | void; reply?: (source: string) => Promise<Response | void> | Response | void; failRead?: () => boolean } = {}) {
  type Entry = { path: string; create_id?: string; source: string; unattached: boolean };
  const manifest: Entry[] = [];
  const listing = () => [...manifest.filter((f) => !f.unattached), ...manifest.filter((f) => f.unattached)]
    .map((f): FileInfo => ({ path: f.path, create_id: f.create_id, type: "image/png", upload: true, unattached: f.unattached, size: 4 }));
  const client = createContentKitClient({
    baseUrl: "https://api.test",
    media: { transport: async () => {}, retries: 0 },
    fetch: (async (input, init) => {
      const path = new URL(String(input)).pathname;
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      if (path.endsWith("/presign")) {
        await o.presign?.((body as PresignBody).path);
        return Response.json({ path: "pages/001.png", source_name: (body as PresignBody).path, blob: `upload-${(body as PresignBody).sha256}`, process_on_upload: true, put: { url: "https://storage.test/file", headers: {} } });
      }
      if (path.endsWith("/commit")) {
        let staged: string | undefined;
        for (const op of (body as CommitBody).ops) {
          if (op.op === "put") {
            if (op.create_id && manifest.some((f) => f.create_id === op.create_id)) continue;
            const entry = { path: `pages/${String(manifest.length + 1).padStart(3, "0")}.png`, create_id: op.create_id, source: op.source_name!, unattached: !!op.unattached };
            manifest.push(entry);
            if (op.unattached) staged = entry.source;
          } else if (op.op === "attach") {
            const i = manifest.findIndex((f) => f.path === op.path && f.unattached);
            if (i >= 0) manifest.push({ ...manifest.splice(i, 1)[0]!, unattached: false });
          }
        }
        // A staging write lands at once; only its reply may be late or lost.
        const reply = staged ? await o.reply?.(staged) : undefined;
        return reply ?? Response.json({ files: listing() });
      }
      if (o.failRead?.()) return Response.json({ code: "unavailable", error: "Unavailable" }, { status: 503 });
      return Response.json({ access: "full", expires: 0, total: 0, offset: 0, limit: 100, files: listing() });
    }) as typeof fetch,
  });
  return { client, manifest, attached: () => manifest.filter((f) => !f.unattached).map((f) => f.source) };
}

function gate() {
  let open!: () => void;
  const opened = new Promise<void>((resolve) => (open = resolve));
  return { opened, open };
}

const lost = () => Response.json({ code: "unavailable", error: "Unavailable" }, { status: 503 });

it("attaches pages in queue order when a later page is still staging as the head commits", async () => {
  const later = gate();
  const replies = new Map(["1.png", "2.png", "3.png"].map((source) => [source, gate()]));
  const s = stagingServer({ presign: (path) => (path === "pages/1.png" ? undefined : later.opened), reply: (source) => replies.get(source)!.opened });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png"), file("2.png"), file("3.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.status).toBe("uploaded"));
    const committed = queue.commit(undefined, { head: true });
    later.open();
    await vi.waitFor(() => expect(queue.getSnapshot().items.every((i) => i.status === "uploaded")).toBe(true));
    replies.forEach((reply) => reply.open());
    await committed;
    await queue.commit(undefined, { head: true });
    expect(s.attached()).toEqual(["1.png", "2.png", "3.png"]);
    expect(s.manifest.some((f) => f.unattached)).toBe(false);
  } finally { queue.dispose(); }
});

it("finds a staged upload whose reply was lost by its create ID and attaches it in queue order", async () => {
  const s = stagingServer({ reply: (source) => (source === "2.png" ? lost() : undefined) });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png"), file("2.png"), file("3.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await vi.waitFor(() => expect(queue.getSnapshot().items.filter((i) => i.unattached)).toHaveLength(2));
    await queue.commit();
    expect(s.attached()).toEqual(["1.png", "2.png", "3.png"]);
    expect(s.manifest.some((f) => f.unattached)).toBe(false);
  } finally { queue.dispose(); }
});

it("marks a lost-reply page that is already attached as committed instead of putting it again", async () => {
  const s = stagingServer({ reply: (source) => (source === "2.png" ? lost() : undefined) });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png"), file("2.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.unattached).toBe(true));
    // Something else attached page 2's staged copy first.
    const two = s.manifest.find((f) => f.source === "2.png")!;
    two.unattached = false;
    const commits: CommitBody[] = [];
    const commit = s.client.media.commit.bind(s.client.media);
    s.client.media.commit = (r, ops, opts) => (commits.push({ ref: r, ops } as CommitBody), commit(r, ops, opts));
    await queue.commit(undefined, { head: true });
    const one = s.manifest.find((f) => f.source === "1.png")!;
    expect(commits.flatMap((c) => c.ops).map((op) => `${op.op} ${op.path}`)).toEqual([`attach ${one.path}`]);
    expect(queue.getSnapshot().items.map((i) => i.status)).toEqual(["committed", "committed"]);
  } finally { queue.dispose(); }
});

it("stops a head batch before a lost-reply page it cannot look up, and places it on the next commit", async () => {
  let readFails = true;
  const s = stagingServer({ reply: (source) => (source === "2.png" ? lost() : undefined), failRead: () => readFails });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png"), file("2.png"), file("3.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().ready).toBe(true));
    await vi.waitFor(() => expect(queue.getSnapshot().items.filter((i) => i.unattached)).toHaveLength(2));
    await queue.commit(undefined, { head: true });
    expect(s.attached()).toEqual(["1.png"]);
    readFails = false;
    await queue.commit(undefined, { head: true });
    expect(s.attached()).toEqual(["1.png", "2.png", "3.png"]);
  } finally { queue.dispose(); }
});

it("does not hold a head commit for pages that finish uploading while it waits", async () => {
  const later = gate();
  const first = gate();
  const s = stagingServer({ presign: (path) => (path === "pages/1.png" ? undefined : later.opened), reply: (source) => (source === "1.png" ? first.opened : new Promise(() => {})) });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png"), file("2.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.status).toBe("uploaded"));
    const committed = queue.commit(undefined, { head: true });
    // Page 2 finishes while the batch waits for page 1; its staging never answers.
    later.open();
    await vi.waitFor(() => expect(queue.getSnapshot().items[1]?.status).toBe("uploaded"));
    first.open();
    await committed;
    expect(s.attached()).toEqual(["1.png"]);
  } finally { queue.dispose(); }
});

it("stops waiting for stagings when the commit is aborted", async () => {
  const s = stagingServer({ reply: () => new Promise(() => {}) });
  const queue = new ItemUploadQueue(s.client.media, { ref, path: "pages/{name}", pollInterval: 100_000 });
  try {
    queue.add([file("1.png")]);
    await vi.waitFor(() => expect(queue.getSnapshot().items[0]?.status).toBe("uploaded"));
    const abort = new AbortController();
    const committed = queue.commit(abort.signal, { head: true });
    abort.abort();
    await expect(committed).rejects.toMatchObject({ code: "aborted" });
  } finally { queue.dispose(); }
});

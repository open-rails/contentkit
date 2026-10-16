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

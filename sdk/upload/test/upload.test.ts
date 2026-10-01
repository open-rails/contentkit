import type { ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { UploadClient, UploadQueue, fetchTransport, fill, publicURL, stem, type Op, type QueueSnapshot, type Transport, type UploadState } from "../src/index.js";
import { bytes } from "./fake.js";
import { KillProxy, startServer, stopServer } from "./server.js";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
const MiB = 1 << 20;
const id = (n: number) => `0192f000-0000-7000-8000-${String(n).padStart(12, "0")}`;
const hex = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");
const png = (seed: number, n = 3000) => new File([bytes(n, seed)], `${seed}.png`, { type: "image/png" });
const put = (up: { path: string; blob: string }): Op => ({ op: "put", path: up.path, blob: up.blob });
const STAGED = /^u-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

describe.skipIf(!endpoint)("uploads and reads against MinIO and the media handlers", () => {
  let proxy: KillProxy;
  let proc: ChildProcess;
  let base: string;
  let namespace: string;

  beforeAll(async () => {
    proxy = new KillProxy(new URL(endpoint!));
    const s = await startServer(await proxy.listen());
    proc = s.proc;
    base = s.url;
    namespace = s.namespace;
  });

  afterAll(async () => {
    if (proc) await stopServer(proc);
    await proxy?.close();
  });

  const client = ({ actor = "alice", upload = "upload", ...o }: { retries?: number; transport?: Transport; actor?: string; upload?: string } = {}) =>
    new UploadClient({
      endpoint: `${base}/${upload}`,
      readEndpoint: `${base}/read`,
      headers: () => ({ "X-Test-Actor": actor }),
      retryDelay: () => 200,
      ...o,
    });

  /** The stored bytes of an item's file (by path or stem) or public name; null when absent. */
  async function stored(ref: { kind: string; id: string }, name: string, at: "path" | "public" = "path") {
    const res = await fetch(`${base}/object?${new URLSearchParams({ ...ref, [at]: name })}`);
    return res.ok ? ((await res.json()) as { size: number; sha256: string }) : null;
  }

  it("stages a page, commits it, and reads the placed blob's derived file as a viewer", async () => {
    const ref = { kind: "gallery", id: id(1) };
    const body = bytes(4096, 7);
    const file = new File([body], "a.png", { type: "image/png" });
    const c = client();
    const up = await c.upload(file, { ref, path: "originals/001.png" });
    expect(up).toMatchObject({ path: "originals/001.png", blob: expect.stringMatching(STAGED), type: "image/png", size: 4096, exists: false });
    const files = await c.commit(ref, [put(up)]);
    expect(files).toMatchObject([{ path: "originals/001.png", type: "image/png", size: 4096, upload: true, staged: true, pending: ["low"] }]);
    const done = await c.waitFor(ref, "originals/001.png", { interval: 100 });
    expect([done.pending, done.staged]).toEqual([undefined, undefined]);
    // Placed at the hash of its bytes: an identical upload now exists.
    expect(await c.upload(file, { ref, path: "originals/001.png" })).toMatchObject({ exists: true, blob: `sha256-${hex(body)}` });

    // A viewer gets the derived page, signed; the upload itself is never served.
    const read = await client({ actor: "reader" }).read(ref);
    expect(read).toMatchObject({ access: "full", total: 1 });
    expect(read.files).toEqual([
      expect.objectContaining({ path: "low-res/001.webp", url: expect.stringMatching(new RegExp(`^http://media\\.invalid/v1/${namespace}/gallery/${ref.id}/private/sha256-${hex(body)}\\?t=`)) }),
    ]);
    expect(await stored(ref, "low-res/001.webp")).toEqual({ size: 4096, sha256: hex(body) });

    const refused = await c.upload(new File([body], "a.gif", { type: "image/gif" }), { ref, path: "originals/002.gif" }).catch((e) => e);
    expect([refused.code, refused.status]).toEqual(["type_not_allowed", 415]);
    expect((await client({ actor: "reader" }).upload(file, { ref, path: "cover" }).catch((e) => e)).code).toBe("forbidden");
  });

  it("names an inline upload on the server and writes its public file", async () => {
    const ref = { kind: "post", id: id(3) };
    const body = bytes(2048, 9);
    const f = await client().put(new File([body], "i.png", { type: "image/png" }), { ref, path: "inline/x.png" });
    expect(f.path).toMatch(/^inline\/i-[0-9a-f-]{36}\.png$/);
    const name = fill("{name}.webp", { name: stem(f.path).slice("inline/".length) });
    expect(publicURL("http://media.invalid", namespace, "post", ref.id, name)).toBe(`http://media.invalid/v1/${namespace}/post/${ref.id}/public/${name}`);
    expect(await stored(ref, name, "public")).toEqual({ size: 2048, sha256: hex(body) });
  });

  it("edits, renames, moves and removes uploads, and refuses more than the upload's cap", async () => {
    const ref = { kind: "post", id: id(4) };
    const c = client();
    const a = await c.upload(png(11), { ref, path: "originals/a.png" });
    const b = await c.upload(png(12), { ref, path: "originals/b.png" });
    await c.commit(ref, [put(a), put(b)]);

    const edit = { crop: { x: 10, y: 20, w: 30, h: 40 }, rotate: 90 };
    let files = await c.commit(ref, [{ op: "edit", path: "originals/a.png", edit }]);
    expect(files[0]).toMatchObject({ path: "originals/a.png", edit });
    expect((await c.commit(ref, [{ op: "edit", path: "originals/a.png", edit: { rotate: 45 } }]).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.commit(ref, [{ op: "edit", path: "originals/a.png" }]))[0]!.edit).toBeUndefined();

    files = await c.commit(ref, [{ op: "rename", path: "originals/a.png", to: "originals/first" }, { op: "move", path: "originals/b.png", index: 0 }]);
    expect(files.map((f) => f.path)).toEqual(["originals/b.png", "originals/first.png"]);

    const third = await c.upload(png(13), { ref, path: "originals/c.png" });
    const refused = await c.commit(ref, [put(third)]).catch((e) => e);
    expect([refused.code, refused.status, refused.isCeiling]).toEqual(["too_many_files", 409, true]);

    files = await c.commit(ref, [{ op: "remove", path: "originals/first.png" }]);
    expect(files.map((f) => f.path)).toEqual(["originals/b.png"]);
    expect((await c.commit(ref, [{ op: "remove", path: "originals/first.png" }]).catch((e) => e)).code).toBe("not_found");
  });

  it("crops a cover: puts it with an edit, re-edits it from its editor view, and removes it with its public files", async () => {
    // 360×240; the cover is 3:1, so a 360-wide crop is 120 high.
    const image = readFileSync(new URL("../e2e/fixtures/small.png", import.meta.url));
    const ref = { kind: "gallery", id: id(5) };
    const puts: string[] = [];
    const counting: Transport = async (req, blob, o) => {
      puts.push(req.url);
      await fetchTransport(req, blob, o);
    };
    const c = client({ transport: counting });
    const f = await c.put(new File([image], "cover.png", { type: "image/png" }), { ref, path: "cover", edit: { crop: { x: 0, y: 40, w: 360, h: 7 } } });
    expect(f).toMatchObject({ path: "cover.png", w: 360, h: 240, edit: { crop: { x: 0, y: 40, w: 360, h: 120 } } });
    expect(puts).toHaveLength(1);
    for (const w of [230, 460]) expect(await stored(ref, `cover-${w}.webp`, "public")).toEqual({ size: image.length, sha256: hex(image) });

    // The first editor read asks the worker for the view; a later one has it.
    const view = await c.editorView(ref, "cover", { interval: 100, timeout: 20_000 });
    expect(view).toMatchObject({ url: expect.stringMatching(new RegExp(`^http://media\\.invalid/v1/${namespace}/gallery/${ref.id}/private/sha256-`)), width: 360, height: 240 });

    // Re-edit the kept upload: nothing is uploaded.
    const turned = { crop: { x: 100, y: 0, w: 80, h: 240 }, rotate: 90 };
    await c.commit(ref, [{ op: "edit", path: f.path, edit: turned }]);
    expect((await c.waitFor(ref, "cover", { interval: 100 })).edit).toEqual(turned);
    expect(puts).toHaveLength(1);
    expect((await c.commit(ref, [{ op: "edit", path: f.path, edit: { rotate: 45 } }]).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.commit(ref, [{ op: "edit", path: "banner", edit: turned }]).catch((e) => e)).code).toBe("not_found");

    // Removal drops the upload; the worker deletes its public names.
    expect(await c.commit(ref, [{ op: "remove", path: f.path }])).toEqual([]);
    await vi.waitFor(async () => expect(await stored(ref, "cover-230.webp", "public")).toBeNull(), { timeout: 10_000, interval: 100 });
    expect((await c.editorView(ref, "cover").catch((e) => e)).code).toBe("not_found");
    expect((await c.commit(ref, [{ op: "remove", path: f.path }]).catch((e) => e)).code).toBe("not_found");
  });

  it("uploads a stale blob again when commit refuses it", async () => {
    // The server's sweep grace is 20 s: presign offers an unreferenced blob as
    // existing while it is fresh, and commit refuses it once it is 15 s old.
    const ref = { kind: "gallery", id: id(2) };
    const file = png(8, 2048);
    const c = client();
    await c.put(file, { ref, path: "originals/001.png" });
    await c.commit(ref, [{ op: "remove", path: "originals/001.png" }]);
    const up = await c.upload(file, { ref, path: "originals/002.png" });
    expect(up).toMatchObject({ exists: true, blob: `sha256-${hex(bytes(2048, 8))}` });
    await new Promise((r) => setTimeout(r, 17_000));
    const refused = await c.commit(ref, [put(up)]).catch((e) => e);
    expect([refused.code, refused.blobs]).toEqual(["not_uploaded", [up.blob]]);
    // Uploaded again, staged, and placed at the same hash.
    const files = await c.commit(ref, [put(up)], { sources: { [up.blob]: file } });
    expect(files).toMatchObject([{ path: "originals/002.png", size: 2048 }]);
    await c.waitFor(ref, "originals/002.png", { interval: 100 });
    expect(await stored(ref, "originals/002.png")).toEqual({ size: 2048, sha256: hex(bytes(2048, 8)) });
  });

  it("survives a killed connection mid-part, resumes and commits a >64 MiB file", async () => {
    const ref = { kind: "video", id: id(1) };
    const body = bytes(72 * MiB + 4321, 9);
    const file = new File([body], "v.mp4", { type: "video/mp4", lastModified: 1 });

    // Session 1: no retries; part 3's connection drops mid-body.
    let saved: UploadState | null = null;
    proxy.kill(3);
    const first = await client({ retries: 0 })
      .upload(file, { ref, path: "source", onState: (s) => (saved = s) })
      .catch((e) => e);
    expect(first.code).toBe("network");
    expect(proxy.kills).toBe(1);
    expect(saved).toMatchObject({ path: "source.mp4", blob: expect.stringMatching(STAGED) });

    // Session 2 (a reload): resume from the saved state; another drop is retried in place.
    let parts = 0;
    const states: UploadState[] = [];
    const counting: Transport = async (req, blob, o) => {
      await fetchTransport(req, blob, o);
      parts++;
    };
    proxy.kill("next");
    const c = client({ transport: counting });
    const up = await c.upload(file, { ref, path: "source", resume: saved!, onState: (s) => s && states.push(s) });
    expect(proxy.kills).toBe(2);
    expect(up).toMatchObject({ path: "source.mp4", blob: saved!.blob, size: body.length, type: "video/mp4" });
    expect(states.at(-1)!.parts.length - parts).toBeGreaterThanOrEqual(1);

    const files = await c.commit(ref, [put(up)]);
    expect(files).toMatchObject([{ path: "source.mp4", size: body.length }]);
    await c.waitFor(ref, "source", { interval: 100 });
    expect(await stored(ref, "source")).toEqual({ size: body.length, sha256: hex(body) });
    expect(await c.upload(file, { ref, path: "source" })).toMatchObject({ exists: true, blob: `sha256-${hex(body)}` });
  });

  it("grabs a still of a video, sets the poster from a frame, then lets the worker choose", async () => {
    const ref = { kind: "video", id: id(7) };
    const c = client();
    // Frames come from the placed video: put waits until it is no longer staged.
    expect((await c.put(new File([bytes(4096, 5)], "v.mp4", { type: "video/mp4" }), { ref, path: "source" })).staged).toBeUndefined();
    const still = await c.getFrame(ref, "source.mp4", 1.5, 320);
    expect([still.type, still.size > 0]).toEqual(["image/jpeg", true]);

    await c.commit(ref, [{ op: "frame", path: "poster", t: 1.5 }]);
    const poster = await c.waitFor(ref, "poster", { interval: 100 });
    expect(poster).toMatchObject({ path: "poster.png", type: "image/png", w: 64, h: 36, frame: { t: 1.5 } });
    expect(await stored(ref, "poster-640.webp", "public")).toMatchObject({ size: poster.size });

    await c.commit(ref, [{ op: "frame", path: "poster", auto: true }]);
    expect((await c.waitFor(ref, "poster", { interval: 100 })).frame).toMatchObject({ auto: true });
  });

  it("processes on upload: stages unattached uploads, attaches them in queue order, discards a removed one", async () => {
    const ref = { kind: "gallery", id: id(6) };
    const c = client({ upload: "upload-on-upload" });
    const q = new UploadQueue(c, { ref, path: "originals/{name}", pollInterval: 100 });
    const until = (ok: (s: QueueSnapshot) => boolean) =>
      new Promise<QueueSnapshot>((resolve) => {
        const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
        const off = q.subscribe(check);
        check();
      });
    const [, b, d] = q.add([png(21, 3000), png(22, 3000), png(23, 3000)].map((f, i) => new File([f], `${"abd"[i]}.png`, { type: f.type })));
    await until((x) => x.items.every((i) => i.unattached && i.processed));
    const uploads = async () => (await c.read(ref, { editor: true })).files.filter((f) => f.upload);
    expect((await uploads()).map((f) => [f.path, f.unattached]).sort()).toEqual([
      ["originals/a.png", true],
      ["originals/b.png", true],
      ["originals/d.png", true],
    ]);

    q.remove(d!.id);
    await vi.waitFor(async () => expect((await uploads()).map((f) => f.path)).not.toContain("originals/d.png"), { timeout: 10_000, interval: 100 });

    q.move(b!.id, 0);
    const files = await q.commit();
    expect(files.map((f) => f.path)).toEqual(["originals/b.png", "originals/a.png"]);
    expect((await uploads()).map((f) => [f.path, !!f.unattached])).toEqual([
      ["originals/b.png", false],
      ["originals/a.png", false],
    ]);
    expect(q.getSnapshot().items.every((i) => i.status === "committed")).toBe(true);
    q.dispose();
  });
});

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { beforeAll, describe, expect, inject, it, vi } from "vitest";
import { UploadQueue, createContentKitClient, fetchTransport, type Op, type QueueSnapshot, type RefBody, type Transport, type UploadState } from "../../src/client/index.js";
import { bytes, png } from "../support/bytes.js";
import { Harness, type Config, type TestUser } from "../support/harness.js";

const MiB = 1 << 20;
const hex = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");
const file = (b: Uint8Array, name: string, type: string, lastModified?: number) => new File([new Uint8Array(b)], name, { type, lastModified });
const image = (seed: number, name = `${seed}.png`) => file(png(48, 32, seed), name, "image/png");
const fixture = (name: string) => readFileSync(new URL(`../fixtures/${name}`, import.meta.url));
const put = (up: { path: string; blob: string }): Op => ({ op: "put", path: up.path, blob: up.blob });
const STAGED = /^u-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const ALLOCATION = /^sha256-[0-9a-f]{64}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

describe("uploads and reads against the real ContentKit", () => {
  const h = new Harness(inject("origin"));
  let cfg: Config;
  let alice: TestUser;
  let reader: TestUser;

  beforeAll(async () => {
    cfg = await h.config();
    [alice, reader] = await Promise.all([h.user(), h.user()]);
  });

  /** The media client of user, against the Runtime mount (or the ProcessOnUpload upload mount). */
  const client = ({ user = alice, onUpload = false, ...media }: { user?: TestUser | null; onUpload?: boolean; retries?: number; concurrency?: number; transport?: Transport; fetch?: typeof fetch } = {}) =>
    createContentKitClient({
      baseUrl: `${h.origin}${cfg.api}`,
      mounts: onUpload ? { upload: `${h.origin}${cfg.on_upload_api}/media/upload` } : undefined,
      token: () => user?.access_token,
      fetch: media.fetch,
      media: { retryDelay: () => 200, ...media },
    }).media;
  const item = async (kind: string, owner = alice): Promise<RefBody> => {
    const it = await h.item({ kind, owner: owner.id });
    return { kind: it.kind, id: it.id };
  };
  const wait = { interval: 200, timeout: 60_000 };

  async function publicNames(ref: RefBody, preset: string) {
    const read = await client().read(ref, { editor: true });
    const image = read.public?.find((p) => p.preset === preset);
    expect(image).toBeDefined();
    return image!.renditions.map((r) => new URL(r.url).pathname.split("/").at(-1)!);
  }

  it("stages a page, commits it, and serves its derived file to a viewer through media-gateway", async () => {
    const ref = await item("gallery");
    const body = png(64, 48, 7);
    const page = file(body, "a.png", "image/png");
    const c = client();
    const up = await c.upload(page, { ref, path: "originals/001.png" });
    expect(up).toMatchObject({ path: "originals/001.png", blob: expect.stringMatching(STAGED), type: "image/png", size: body.length, exists: false });
    const files = await c.commit(ref, [put(up)]);
    expect(files).toMatchObject([{ path: "originals/001.png", type: "image/png", size: body.length, upload: true, staged: true, pending: ["low"] }]);
    const done = await c.waitFor(ref, "originals/001.png", wait);
    expect([done.pending, done.staged, done.w, done.h]).toEqual([undefined, undefined, 64, 48]);
    // The current identical allocation is returned, rather than uploaded again.
    const again = await c.upload(page, { ref, path: "originals/001.png" });
    expect(again).toMatchObject({ exists: true, blob: expect.stringMatching(ALLOCATION) });
    expect(again.blob.startsWith(`sha256-${hex(body)}-`)).toBe(true);
    expect((await c.upload(page, { ref, path: "originals/001.png" })).blob).toBe(again.blob);

    // A viewer gets the derived page, signed; the upload itself is never served.
    const read = await client({ user: reader }).read(ref);
    expect(read).toMatchObject({ access: "full", total: 1 });
    expect(read.files).toEqual([
      expect.objectContaining({ path: "low-res/001.webp", url: expect.stringMatching(new RegExp(`^${cfg.media}/v1/${cfg.namespace}/gallery/${ref.id}/private/sha256-[0-9a-f]{64}-[0-9a-f-]{36}\\?t=`)) }),
    ]);
    const served = await fetch(read.files[0]!.url!);
    expect([served.status, served.headers.get("content-type")]).toEqual([200, "image/webp"]);
    const webp = new Uint8Array(await served.arrayBuffer());
    expect(new TextDecoder().decode(webp.subarray(8, 12))).toBe("WEBP");
    expect(await h.object(ref, "low-res/001.webp")).toEqual({ size: webp.length, sha256: hex(webp) });

    const refused = await c.upload(file(body, "a.gif", "image/gif"), { ref, path: "originals/002.gif" }).catch((e) => e);
    expect([refused.code, refused.status]).toEqual(["type_not_allowed", 415]);
    expect((await client({ user: reader }).upload(page, { ref, path: "cover" }).catch((e) => e)).code).toBe("forbidden");
    expect((await client({ user: null }).upload(page, { ref, path: "cover" }).catch((e) => e)).status).toBe(401);
  });

  it("names an inline upload on the server and publishes it", async () => {
    const ref = await item("note");
    const f = await client().put(file(fixture("small.png"), "i.png", "image/png"), { ref, path: "inline/x.png" });
    expect(f.path).toMatch(/^inline\/i-[0-9a-f-]{36}\.png$/);
    await client().waitFor(ref, f.path, wait);
    const [name] = await publicNames(ref, "inline");
    expect(name).toMatch(/-[0-9a-f-]{36}\.webp$/);
    expect((await h.object(ref, name!, "public"))?.size).toBeGreaterThan(0);
  });

  it("edits, renames, moves and removes uploads, and refuses more than the upload's cap", async () => {
    const ref = await item("note");
    const c = client();
    const a = await c.upload(image(11), { ref, path: "originals/a.png" });
    const b = await c.upload(image(12), { ref, path: "originals/b.png" });
    await c.commit(ref, [put(a), put(b)]);

    const edit = { crop: { x: 10, y: 4, w: 30, h: 20 }, rotate: 90 };
    let files = await c.commit(ref, [{ op: "edit", path: "originals/a.png", edit }]);
    expect(files[0]).toMatchObject({ path: "originals/a.png", edit });
    expect((await c.commit(ref, [{ op: "edit", path: "originals/a.png", edit: { rotate: 45 } }]).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.commit(ref, [{ op: "edit", path: "originals/a.png" }]))[0]!.edit).toBeUndefined();

    files = await c.commit(ref, [{ op: "rename", path: "originals/a.png", to: "originals/first" }, { op: "move", path: "originals/b.png", index: 0 }]);
    expect(files.map((f) => f.path)).toEqual(["originals/b.png", "originals/first.png"]);

    const third = await c.upload(image(13), { ref, path: "originals/c.png" });
    const refused = await c.commit(ref, [put(third)]).catch((e) => e);
    expect([refused.code, refused.status, refused.isCeiling]).toEqual(["too_many_files", 409, true]);

    // A commit whose answer is lost is sent again, byte for byte, and applied once.
    let lost = false;
    const requests: string[] = [];
    const recovering = client({
      fetch: async (input, init) => {
        requests.push(String(init?.body));
        const res = await fetch(input, init);
        if (!lost && res.ok && init?.method === "POST") {
          lost = true;
          await res.body?.cancel();
          throw new Error("lost successful commit response");
        }
        return res;
      },
    });
    const operationID = crypto.randomUUID();
    const removal: Op[] = [{ op: "remove", path: "originals/first.png" }];
    files = await recovering.commit(ref, removal, { operationID });
    expect(requests).toHaveLength(2);
    expect(requests[0]).toBe(requests[1]);
    expect(files.map((f) => f.path)).toEqual(["originals/b.png"]);
    expect(await c.commit(ref, removal, { operationID })).toEqual(files);
    expect((await c.commit(ref, [{ op: "remove", path: "originals/b.png" }], { operationID }).catch((e) => e)).code).toBe("conflict");
    expect((await c.commit(ref, [{ op: "remove", path: "originals/first.png" }]).catch((e) => e)).code).toBe("not_found");
  });

  it("crops a cover: puts it with an edit, re-edits it from its editor view, and removes it with its public files", async () => {
    // 360×240; the cover is 3:1, so a 360-wide crop is 120 high.
    const ref = await item("gallery");
    const puts: string[] = [];
    const counting: Transport = async (req, blob, o) => {
      puts.push(req.url);
      await fetchTransport(req, blob, o);
    };
    const c = client({ transport: counting });
    const f = await c.put(file(fixture("small.png"), "cover.png", "image/png"), { ref, path: "cover", edit: { crop: { x: 0, y: 40, w: 360, h: 7 } } });
    expect(f).toMatchObject({ path: "cover.png", w: 360, h: 240, edit: { crop: { x: 0, y: 40, w: 360, h: 120 } } });
    expect(puts).toHaveLength(1);
    await c.waitFor(ref, "cover", wait);
    const oldNames = await publicNames(ref, "cover");
    expect(oldNames.length).toBeGreaterThan(0);
    for (const name of oldNames) expect((await h.object(ref, name, "public"))?.size).toBeGreaterThan(0);

    // The first editor read asks the worker for the view; a later one has it.
    const view = await c.editorView(ref, "cover", wait);
    expect(view).toMatchObject({ url: expect.stringMatching(new RegExp(`^${cfg.media}/v1/${cfg.namespace}/gallery/${ref.id}/private/sha256-`)), width: 360, height: 240 });
    expect((await fetch(view.url)).status).toBe(200);

    // Re-edit the kept upload: nothing is uploaded.
    const turned = { crop: { x: 100, y: 0, w: 80, h: 240 }, rotate: 90 };
    await c.commit(ref, [{ op: "edit", path: f.path, edit: turned }]);
    expect((await c.waitFor(ref, "cover", wait)).edit).toEqual(turned);
    const names = await publicNames(ref, "cover");
    expect(names.length).toBeGreaterThan(0);
    expect(names.some((name) => oldNames.includes(name))).toBe(false);
    await vi.waitFor(async () => {
      for (const name of oldNames) expect(await h.object(ref, name, "public")).toBeNull();
    }, { timeout: 30_000, interval: 250 });
    expect(puts).toHaveLength(1);
    expect((await c.commit(ref, [{ op: "edit", path: f.path, edit: { rotate: 45 } }]).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.commit(ref, [{ op: "edit", path: "banner", edit: turned }]).catch((e) => e)).code).toBe("not_found");

    // Removal confirms public cleanup before returning; retries are harmless.
    expect(await c.commit(ref, [{ op: "remove", path: f.path }])).toEqual([]);
    for (const name of names) expect(await h.object(ref, name, "public")).toBeNull();
    expect((await c.editorView(ref, "cover").catch((e) => e)).code).toBe("not_found");
    expect(await c.commit(ref, [{ op: "remove", path: f.path }])).toEqual([]);
  });

  it("uploads a stale blob again when commit refuses it", async () => {
    // Presign can reuse only a current allocation. Removing its last reference
    // before commit makes the old offer unusable, regardless of object age.
    const ref = await item("gallery");
    const body = png(40, 30, 8);
    const page = file(body, "p.png", "image/png");
    const c = client();
    await c.put(page, { ref, path: "originals/001.png" });
    const up = await c.upload(page, { ref, path: "originals/002.png" });
    expect(up).toMatchObject({ exists: true, blob: expect.stringMatching(ALLOCATION) });
    await c.commit(ref, [{ op: "remove", path: "originals/001.png" }]);
    const refused = await c.commit(ref, [put(up)]).catch((e) => e);
    expect([refused.code, refused.blobs]).toEqual(["not_uploaded", [up.blob]]);
    // Recovery stages the same bytes under a fresh allocation.
    const files = await c.commit(ref, [put(up)], { sources: { [up.blob]: page } });
    expect(files).toMatchObject([{ path: "originals/002.png", size: body.length }]);
    await c.waitFor(ref, "originals/002.png", wait);
    expect(await h.object(ref, "originals/002.png")).toEqual({ size: body.length, sha256: hex(body) });
  });

  it("survives a dropped connection mid-part, resumes and commits a >64 MiB file", async () => {
    const ref = await item("file");
    const body = bytes(72 * MiB + 4321, 9);
    const big = file(body, "v.bin", "application/octet-stream", 1);

    // Session 1: no retries, one PUT at a time; the second part's connection drops mid-body.
    let saved: UploadState | null = null;
    await h.faults([{ item: ref.id, fault: "drop", skip: 1, after: 2 * MiB, times: 1 }]);
    const first = await client({ retries: 0, concurrency: 1 })
      .upload(big, { ref, path: "file", onState: (s) => (saved = s) })
      .catch((e) => e);
    expect(first.code).toBe("network");
    expect(saved).toMatchObject({ path: "file.bin", blob: expect.stringMatching(STAGED) });

    // Session 2 (a reload): resume from the saved state; another drop is retried in place.
    let parts = 0;
    const states: UploadState[] = [];
    const counting: Transport = async (req, blob, o) => {
      await fetchTransport(req, blob, o);
      parts++;
    };
    await h.faults([{ item: ref.id, fault: "drop", after: 2 * MiB, times: 1 }]);
    const c = client({ transport: counting });
    const up = await c.upload(big, { ref, path: "file", resume: saved!, onState: (s) => s && states.push(s) });
    expect((await h.listFaults()).filter((f) => f.item === ref.id).map((f) => f.hits)).toEqual([1, 1]);
    expect(up).toMatchObject({ path: "file.bin", blob: saved!.blob, size: body.length, type: "application/octet-stream" });
    expect(states.at(-1)!.parts.length - parts).toBeGreaterThanOrEqual(1);

    const files = await c.commit(ref, [put(up)]);
    expect(files).toMatchObject([{ path: "file.bin", size: body.length }]);
    await c.waitFor(ref, "file", wait);
    expect(await h.object(ref, "file")).toEqual({ size: body.length, sha256: hex(body) });
    const again = await c.upload(big, { ref, path: "file" });
    expect(again).toMatchObject({ exists: true, blob: expect.stringMatching(ALLOCATION) });
    expect(again.blob.startsWith(`sha256-${hex(body)}-`)).toBe(true);
    await h.clearFaults(ref.id);
  });

  it("grabs a still of a video, sets the poster from a frame, then lets the worker choose", async () => {
    const ref = await item("video");
    const c = client();
    // Frames come from the placed video: put waits until it is no longer staged.
    expect((await c.put(file(fixture("clip.mp4"), "v.mp4", "video/mp4"), { ref, path: "source" })).staged).toBeUndefined();
    const still = await c.getFrame(ref, "source.mp4", 1.5, 320);
    expect([still.type, still.size > 0]).toEqual(["image/jpeg", true]);

    await c.commit(ref, [{ op: "frame", path: "poster", t: 1.5 }]);
    const poster = await c.waitFor(ref, "poster", wait);
    expect(poster).toMatchObject({ path: "poster.png", type: "image/png", w: 640, h: 360, frame: { t: 1.5 } });
    const [name] = await publicNames(ref, "poster");
    expect((await h.object(ref, name!, "public"))?.size).toBeGreaterThan(0);

    await c.commit(ref, [{ op: "frame", path: "poster", auto: true }]);
    expect((await c.waitFor(ref, "poster", wait)).frame).toMatchObject({ auto: true });
  });

  it("processes on upload: stages unattached uploads, attaches them in queue order, discards a removed one", async () => {
    const ref = await item("gallery");
    const c = client({ onUpload: true });
    const q = new UploadQueue(c, { ref, path: "originals/{name}", pollInterval: 200 });
    const until = (ok: (s: QueueSnapshot) => boolean) =>
      new Promise<QueueSnapshot>((resolve) => {
        const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
        const off = q.subscribe(check);
        check();
      });
    const [, b, d] = q.add([21, 22, 23].map((seed, i) => image(seed, `${"abd"[i]}.png`)));
    await until((x) => x.items.every((i) => i.unattached && i.processed));
    const uploads = async () => (await c.read(ref, { editor: true })).files.filter((f) => f.upload);
    expect((await uploads()).map((f) => [f.path, f.unattached]).sort()).toEqual([
      ["originals/a.png", true],
      ["originals/b.png", true],
      ["originals/d.png", true],
    ]);

    q.remove(d!.id);
    await vi.waitFor(async () => expect((await uploads()).map((f) => f.path)).not.toContain("originals/d.png"), { timeout: 30_000, interval: 200 });

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

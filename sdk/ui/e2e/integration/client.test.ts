import { File as NodeFile } from "node:buffer";
import { createHash } from "node:crypto";
import { beforeAll, describe, expect, it } from "vitest";
import { ContentKitError, createContentKitClient, fetchTransport, type CommitBody, type RefBody, type UploadState } from "../../src/client/index.js";
import { bytes, png as pngBytes } from "../support/bytes.js";
import type { Config, TestUser } from "../support/harness.js";
import { Accounts, client, fixture, harness, item, recorder, wait, type ClientOptions, type Recorder } from "./setup.js";

const MiB = 1 << 20;
const STAGED = /^u-[0-9a-f-]{36}$/;
const ALLOCATION = /^sha256-[0-9a-f]{64}-[0-9a-f-]{36}$/;
const hex = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const asFile = (b: Uint8Array, name: string, type: string) => new NodeFile([b], name, { type, lastModified: 1000 }) as unknown as File;

const h = harness();
const accounts = new Accounts(h);
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  await accounts.load("owner");
  owner = accounts.get("owner");
});

/** The owner's media client, recording into rec. */
const media = (rec?: Recorder, o: ClientOptions = {}) => client(h, cfg, owner, { record: rec, ...o }).media;
/** Raw bytes for the `file` kind (application/octet-stream, no processing). */
const raw = (n: number, seed = 1) => asFile(bytes(n, seed), `f${seed}.bin`, "application/octet-stream");
const image = (seed: number, name = `${seed}.png`, w = 48, hgt = 32) => asFile(pngBytes(w, hgt, seed), name, "image/png");
const reads = (rec: Recorder) => rec.calls.filter((x) => x === "/read").length;

/**
 * An exists offer at path for the bytes of src that commit then refuses as
 * stale: the bytes go in once at keep, the same bytes offer that allocation
 * at path, and removing keep before the commit leaves the offer unusable.
 */
async function staleOffer(ref: RefBody, src: File, path: string) {
  const setup = media();
  const keep = await setup.put(src, { ref, path: path.replace(/(\.\w+)?$/, "-keep$1") });
  const up = await setup.upload(src, { ref, path });
  expect(up).toMatchObject({ exists: true, blob: expect.stringMatching(ALLOCATION) });
  await setup.commit(ref, [{ op: "remove", path: keep.path }]);
  return up;
}

describe("single PUT", () => {
  it("retries a lost commit response with one immutable batch and preserves later edits", async () => {
    const ref = await item(h, "gallery", owner);
    await media().put(image(1, "source.png"), { ref, path: "originals/source.png" });
    const rec = recorder();
    const ops = [{ op: "rename" as const, path: "originals/source.png", to: "originals/renamed.png" }];
    let dropped = false;
    const c = createContentKitClient({
      baseUrl: `${h.origin}${cfg.api}`,
      token: () => owner.access_token,
      media: { retryDelay: () => 0, transport: fetchTransport },
      fetch: async (input, init) => {
        const res = await rec.fetch(input, init);
        if (!dropped && String(input).endsWith("/commit")) {
          dropped = true;
          ops[0]!.to = "originals/mutated.png";
          throw new Error("response lost after commit");
        }
        return res;
      },
    }).media;
    const id = crypto.randomUUID();
    expect((await c.commit(ref, ops, { operationID: id }))[0]!.path).toBe("originals/renamed.png");
    expect(rec.commitRequests[0]).toEqual(rec.commitRequests[1]);
    // A later edit, then the same operation again: answered from its record, not applied twice.
    await media().commit(ref, [{ op: "rename", path: "originals/renamed.png", to: "originals/later.png" }]);
    const original = rec.commitRequests[0]!.ops;
    expect((await c.commit(ref, original, { operationID: id }))[0]!.path).toBe("originals/later.png");
    await expect(c.commit(ref, ops, { operationID: id })).rejects.toMatchObject({ code: "conflict" });
    expect((await media().read(ref, { editor: true })).files.filter((f) => f.upload).map((f) => f.path)).toEqual(["originals/later.png"]);
  });

  it("hashes, presigns the path with the checksum and stages one PUT; a placed identical file is not resent", async () => {
    const ref = await item(h, "gallery", owner);
    const rec = recorder();
    const c = media(rec);
    const body = pngBytes(1024, 1024, 2); // about 3 MiB of noise
    const f = asFile(body, "cover.png", "image/png");
    const phases = new Set<string>();
    const up = await c.upload(f, { ref, path: "cover", onProgress: (p) => phases.add(p.phase) });
    expect(up).toMatchObject({ path: "cover.png", blob: expect.stringMatching(STAGED), type: "image/png", size: body.length, exists: false });
    expect(rec.presigns[0]).toMatchObject({ path: "cover", type: "image/png", size: body.length, sha256: hex(body) });
    expect([rec.puts, [...phases]]).toEqual([[expect.stringContaining(up.blob)], ["hashing", "uploading"]]);
    await c.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    await c.waitFor(ref, "cover", wait);
    const again = await c.upload(f, { ref, path: "cover" });
    expect(again).toMatchObject({ exists: true, blob: expect.stringMatching(ALLOCATION) });
    expect([again.blob.startsWith(`sha256-${hex(body)}-`), rec.puts.length]).toEqual([true, 1]);
  });

  it("retries a dropped PUT", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    await h.faults([{ item: ref.id, fault: "drop", times: 2 }]);
    try {
      await media(rec).upload(raw(MiB), { ref, path: "file" });
    } finally {
      await h.clearFaults(ref.id);
    }
    expect(rec.puts.length).toBe(3);
  });

  it("surfaces a refused presign without sending bytes", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    await h.faults([{ item: ref.id, fault: "api", path: "/media/upload/presign", status: 429, code: "rate_limited", error: "too many uploads", retry_after: 60 }]);
    let err;
    try {
      err = await media(rec).upload(raw(MiB), { ref, path: "file" }).catch((e) => e);
    } finally {
      await h.clearFaults(ref.id);
    }
    expect(err).toBeInstanceOf(ContentKitError);
    expect([err.code, err.retryAfter, err.isLimit, rec.puts.length]).toEqual(["rate_limited", 60, true, 0]);
  });

  it("puts an upload under the path the server names, and waits until it is processed", async () => {
    const ref = await item(h, "note", owner);
    const rec = recorder();
    const phases: string[] = [];
    const f = await media(rec).put(image(4, "x.png"), { ref, path: "inline/x.png", createOnly: true, edit: { rotate: 90 }, onProgress: (p) => phases.push(p.phase) });
    expect(f).toMatchObject({ path: expect.stringMatching(/^inline\/i-[0-9a-f-]{36}\.png$/), upload: true, edit: { rotate: 90 } });
    expect(f.pending).toBeUndefined();
    expect(rec.commits[0]).toEqual([{ op: "put", path: f.path, blob: expect.stringMatching(STAGED), create_id: expect.any(String), edit: { rotate: 90 } }]);
    expect(phases.at(-1)).toBe("processing");
    expect(reads(rec)).toBeGreaterThanOrEqual(1);
  });
});

describe("multipart", () => {
  const size = 70 * MiB + 123;

  it("hashes the whole file first, then stages it in parts", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    const c = media(rec);
    const phases: string[] = [];
    const up = await c.upload(raw(size), { ref, path: "file", onProgress: (p) => phases.at(-1) !== p.phase && phases.push(p.phase) });
    expect(phases).toEqual(["hashing", "uploading", "completing"]);
    expect(rec.presigns[0]!.sha256).toBe(hex(bytes(size, 1)));
    expect(up).toMatchObject({ path: "file.bin", blob: expect.stringMatching(STAGED) });
    await c.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    await c.waitFor(ref, "file", wait);
    expect((await h.object(ref, "file"))?.size).toBe(size);
  });

  it("refuses before any part moves", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    await h.faults([{ item: ref.id, fault: "api", path: "/media/upload/presign", status: 413, code: "quota_exceeded", error: "over quota" }]);
    let err;
    try {
      err = await media(rec).upload(raw(size), { ref, path: "file" }).catch((e) => e);
    } finally {
      await h.clearFaults(ref.id);
    }
    expect([err.code, rec.puts.length, rec.calls]).toEqual(["quota_exceeded", 0, ["/presign"]]);
  });

  it("uploads parts within the bounds, retries a dropped part and completes", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    await h.faults([{ item: ref.id, fault: "drop", times: 1 }]);
    const states: (UploadState | null)[] = [];
    let last = 0;
    let up;
    try {
      up = await media(rec).upload(raw(size), { ref, path: "file", onState: (st) => states.push(st), onProgress: (p) => (last = p.loaded) });
    } finally {
      await h.clearFaults(ref.id);
    }
    expect(up).toMatchObject({ size, exists: false });
    expect(states.at(-1)).toBeNull();
    const { parts: plan, limits } = states.at(-2)!;
    expect(plan.reduce((n, p) => n + p.size, 0)).toBe(size);
    for (const p of plan.slice(0, -1)) expect(p.size).toBeGreaterThanOrEqual(limits.minPartSize);
    for (const p of plan) expect(p.size).toBeLessThanOrEqual(limits.maxPartSize);
    expect(rec.puts.length).toBe(plan.length + 1);
    expect(last).toBe(size);
  });

  it("presigns parts ahead of the PUTs but never exceeds the PUT concurrency", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    let active = 0;
    let peak = 0;
    let ahead = 0;
    let started = 0;
    const c = client(h, cfg, owner, {
      record: rec,
      media: {
        concurrency: 2,
        transport: async (req, body, o) => {
          peak = Math.max(peak, ++active);
          started++;
          await sleep(20);
          ahead = Math.max(ahead, rec.calls.filter((p) => p === "/parts").length - started);
          try {
            await fetchTransport(req, body, o);
          } finally {
            active--;
          }
        },
      },
    }).media;
    await c.upload(raw(size, 3), { ref, path: "file" });
    expect(peak).toBe(2);
    expect(ahead).toBeGreaterThan(0);
  });

  it("resumes from saved state, sending only the parts that did not land", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    const c = media(rec, { media: { retries: 0, concurrency: 1 } });
    const f = raw(size, 5);
    let saved: UploadState | null = null;
    let landed = 0;
    const ctl = new AbortController();
    const first = c.upload(f, {
      ref,
      path: "file",
      signal: ctl.signal,
      onState: (st) => (saved = st),
      onProgress: (p) => {
        if (p.phase === "uploading" && p.loaded >= 16 * MiB && !ctl.signal.aborted) {
          landed = p.loaded;
          ctl.abort();
        }
      },
    });
    expect((await first.catch((e) => e)).code).toBe("aborted");
    expect(saved).not.toBeNull();

    const landedParts = (await c.api.listParts(ref, { ticket: saved!.ticket })).parts.length;
    expect(landedParts).toBeGreaterThan(0);
    expect(landed).toBeGreaterThanOrEqual(16 * MiB);
    const resumedRec = recorder();
    let final: UploadState | null = null;
    const resumed = await media(resumedRec).upload(f, { ref, path: "file", resume: saved!, onState: (st) => st && (final = st) });
    expect(resumed).toMatchObject({ path: saved!.path, blob: saved!.blob, size });
    expect(resumedRec.puts.length).toBe(final!.parts.length - landedParts);
    expect(resumedRec.calls).toContain("/parts/list");
  });

  it("refuses to resume with a different file", async () => {
    const ref = await item(h, "file", owner);
    const state = { ticket: "t", path: "file.bin", blob: "u-x", type: "application/octet-stream", size, ref, file: { size, name: "other", lastModified: 1 }, limits: { minPartSize: 8 * MiB, maxPartSize: 16 * MiB, maxParts: 9 }, parts: [] };
    const err = await media().upload(raw(size), { ref, path: "file", resume: state }).catch((e) => e);
    expect(err.code).toBe("resume_mismatch");
  });

  it("stops every part when one fails for good", async () => {
    const ref = await item(h, "file", owner);
    const rec = recorder();
    await h.faults([{ item: ref.id, fault: "drop" }]);
    let err;
    try {
      err = await media(rec, { media: { retries: 1 } }).upload(raw(size), { ref, path: "file" }).catch((e) => e);
    } finally {
      await h.clearFaults(ref.id);
    }
    expect(err.code).toBe("network");
    // Before the failing part's second PUT, other parts may take the slot
    // (how many depends on hashing speed). The deterministic bounds: at most
    // pacer concurrency (1) + 2 parts in flight, one PUT each, and the part
    // that failed for good PUT last; upload() settles every part first.
    const part = (u: string) => new URL(u).searchParams.get("partNumber")!;
    const tries = new Map<string, number>();
    for (const u of rec.puts) tries.set(part(u), (tries.get(part(u)) ?? 0) + 1);
    expect(tries.size).toBeLessThanOrEqual(3);
    expect([...tries.values()].filter((n) => n === 2).length).toBeGreaterThanOrEqual(1);
    expect([...tries.values()].every((n) => n <= 2)).toBe(true);
    expect(tries.get(part(rec.puts.at(-1)!))).toBe(2);
  });
});

describe("commit", () => {
  const put = (path: string, blob: string) => ({ op: "put" as const, path, blob });

  it("resumes a replacement batch after its successful response is lost beyond the retry budget", async () => {
    const gallery = await item(h, "gallery", owner);
    await media().put(image(17, "old.png"), { ref: gallery, path: "originals/old.png" });
    const source = image(18, "new.png");
    const uploaded = await staleOffer(gallery, source, "originals/new.png");
    const rec = recorder();
    let loseResponses = true;
    const c = createContentKitClient({
      baseUrl: `${h.origin}${cfg.api}`,
      token: () => owner.access_token,
      media: { retries: 1, retryDelay: () => 0, transport: rec.transport },
      fetch: async (input, init) => {
        const response = await rec.fetch(input, init);
        if (loseResponses && String(input).endsWith("/commit") && response.ok) throw new Error("response lost");
        return response;
      },
    }).media;
    const states: CommitBody[] = [];
    await expect(c.commit(gallery, [
      { op: "rename", path: "originals/old.png", to: "originals/renamed.png" },
      put(uploaded.path, uploaded.blob),
    ], { sources: { [uploaded.blob]: source }, onState: (body) => states.push(body) })).rejects.toMatchObject({ code: "network" });
    expect(states).toHaveLength(2);
    expect(states[1]!.operation_id).not.toBe(states[0]!.operation_id);
    expect(states[1]!.ops[1]!.blob).not.toBe(uploaded.blob);
    loseResponses = false;
    const active = states[1]!;
    const files = await c.commit(active.ref, active.ops, { operationID: active.operation_id });
    // A new upload joins its path in name order.
    expect(files.map((f) => f.path)).toEqual(["originals/new.png", "originals/renamed.png"]);
    // The replacement applied once, and every later request was that batch, unchanged.
    expect((await media().read(gallery, { editor: true })).files.filter((f) => f.upload).map((f) => f.path)).toEqual(["originals/new.png", "originals/renamed.png"]);
    const replays = rec.commitRequests.filter((body) => body.operation_id === active.operation_id);
    expect(replays.length).toBeGreaterThanOrEqual(2);
    expect(replays.every((body) => JSON.stringify(body) === JSON.stringify(active))).toBe(true);
  });

  it("uploads a file again when its blob is due for cleanup, then commits once more", async () => {
    const gallery = await item(h, "gallery", owner);
    const a = image(11, "1.png");
    const b = image(12, "2.png");
    const ub = await staleOffer(gallery, b, "originals/2.png");
    const rec = recorder();
    const c = media(rec);
    const ua = await c.upload(a, { ref: gallery, path: "originals/1.png" });
    const commits = () => rec.calls.filter((x) => x === "/commit").length;
    const files = await c.commit(gallery, [put(ua.path, ua.blob), put(ub.path, ub.blob)], { sources: { [ua.blob]: a, [ub.blob]: b } });
    expect(files.map((f) => f.path)).toEqual(["originals/1.png", "originals/2.png"]);
    expect([commits(), rec.puts.length]).toEqual([2, 2]); // a's PUT, then only b again, staged anew
    const retried = rec.commits.at(-1)!.map((op) => op.blob);
    expect(retried[0]).toBe(ua.blob);
    expect(retried[1]).toMatch(STAGED);
    expect(retried[1]).not.toBe(ub.blob);
  });

  it("gives up after one retry, and without sources", async () => {
    const gallery = await item(h, "gallery", owner);
    const a = image(13, "1.png");
    const ua = await staleOffer(gallery, a, "originals/1.png");
    const ops = [put(ua.path, ua.blob)];
    const rec = recorder();
    expect((await media(rec).commit(gallery, ops).catch((e) => e)).code).toBe("not_uploaded");
    // The PUT reports success but nothing lands: the server refuses the fresh staged name too.
    const c2 = client(h, cfg, owner, { record: rec, media: { retryDelay: () => 0, transport: async () => {} } }).media;
    const err = await c2.commit(gallery, ops, { sources: { [ua.blob]: a } }).catch((e) => e);
    expect(err.code).toBe("not_uploaded");
    expect(err.blobs).toEqual([expect.stringMatching(STAGED)]);
    expect(err.blobs[0]).not.toBe(ua.blob);
    expect(rec.calls.filter((x) => x === "/commit").length).toBe(3);
  });
});

describe("reads", () => {
  // 600×400: a 100×300 crop turned a quarter is the cover's 3:1.
  const edit = { crop: { x: 200, y: 0, w: 100, h: 300 }, rotate: 90 };

  it("waits for an upload by path or stem, and rejects on its failure or absence", async () => {
    const ref = await item(h, "gallery", owner);
    const rec = recorder();
    const c = media(rec);
    await c.put(image(5, "c.png", 600, 400), { ref, path: "cover" });
    // Pending: the worker is held on this item until released.
    await h.faults([{ item: ref.id, fault: "hold" }]);
    try {
      await c.commit(ref, [{ op: "edit", path: "cover.png", edit }]);
      const done = c.waitFor(ref, "cover", { interval: 50, timeout: 60_000 });
      await sleep(400);
      expect(reads(rec)).toBeGreaterThanOrEqual(2);
      await h.clearFaults(ref.id);
      expect(await done).toMatchObject({ path: "cover.png", edit });

      // A new upload with nothing pending is not processed while it is staged.
      await h.faults([{ item: ref.id, fault: "hold" }]);
      const before = reads(rec);
      const up = await c.upload(image(6, "p.png"), { ref, path: "originals/p.png" });
      await c.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
      const placed = c.waitFor(ref, "originals/p", { interval: 50, timeout: 60_000 });
      await sleep(400);
      expect(reads(rec) - before).toBeGreaterThanOrEqual(2);
      await h.clearFaults(ref.id);
      expect(await placed).toMatchObject({ path: "originals/p.png" });
    } finally {
      await h.clearFaults(ref.id);
    }

    // An image the worker cannot read fails, and says why.
    const bad = await c.upload(asFile(bytes(4096, 9), "bad.png", "image/png"), { ref, path: "originals/bad.png" });
    await c.commit(ref, [{ op: "put", path: bad.path, blob: bad.blob }]);
    const failed = await c.waitFor(ref, "originals/bad", wait).catch((e) => e);
    expect(failed).toBeInstanceOf(ContentKitError);
    expect([failed.code, failed.refusal]).toEqual(["image_unreadable", true]);
    expect((await c.waitFor(ref, "banner").catch((e) => e)).code).toBe("not_found");

    // A staged video source, and a poster frame still rendering, time out.
    const video = await item(h, "video", owner);
    await h.faults([{ item: video.id, fault: "hold" }]);
    try {
      const src = await c.upload(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref: video, path: "source" });
      await c.commit(video, [{ op: "put", path: src.path, blob: src.blob }]);
      expect((await c.waitFor(video, "source", { timeout: 0 }).catch((e) => e)).code).toBe("render_timeout");
      await h.clearFaults(video.id);
      await c.waitFor(video, "source", wait);
      await h.faults([{ item: video.id, fault: "hold" }]);
      await c.commit(video, [{ op: "frame", path: "poster", t: 3 }]);
      expect((await c.waitFor(video, "poster", { timeout: 0 }).catch((e) => e)).code).toBe("render_timeout");
    } finally {
      await h.clearFaults(video.id);
    }
  });

  it("finds the editor view of an upload for re-cropping", async () => {
    const ref = await item(h, "gallery", owner);
    const rec = recorder();
    const c = media(rec);
    // 1280×720; a 1200×400 crop is the cover's 3:1.
    const crop = { crop: { x: 40, y: 160, w: 1200, h: 400 } };
    await c.put(fixture("media/img/3.jpg", "image/jpeg", "c.jpg"), { ref, path: "cover", edit: crop });
    expect(await c.editorView(ref, "cover", wait)).toEqual({
      url: expect.stringMatching(new RegExp(`^${cfg.media}/v1/${cfg.namespace}/gallery/${ref.id}/private/sha256-`)),
      width: 1280,
      height: 720,
      edit: crop,
    });
    expect((await c.editorView(ref, "banner").catch((e) => e)).code).toBe("not_found");
    expect(rec.commits[0]![0]).toMatchObject({ op: "put", path: "cover.jpg", edit: crop });
  });

  it("reads with options as query flags, and builds HLS and frame URLs", async () => {
    const ref = await item(h, "video", owner);
    await media().put(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref, path: "source" });
    const urls: string[] = [];
    const spy = client(h, cfg, owner, { record: { ...recorder(), fetch: (async (u: string, i: RequestInit) => (urls.push(String(u)), fetch(u, i))) as typeof fetch } }).media;
    await spy.read(ref, { prefix: "low-res/", offset: 50, limit: 25, download: true, editor: true });
    const still = await spy.getFrame(ref, "source.mp4", 1.5, 320);
    expect([still.type, still.size > 0]).toEqual(["image/jpeg", true]);
    const api = `${h.origin}${cfg.api}`;
    expect(urls).toEqual([
      `${api}/media/video/${ref.id}?prefix=low-res%2F&offset=50&limit=25&download=1&editor=1`,
      `${api}/media/upload/frame?kind=video&id=${ref.id}&path=source.mp4&t=1.5&w=320`,
    ]);
    expect(spy.hlsBase(ref, "hls/")).toBe(`${api}/media/video/${ref.id}/hls/hls/`);
    expect(createContentKitClient({ baseUrl: "http://x", mounts: { upload: "/u" } }).media.hlsBase(ref, "hls/")).toBe(`http://x/media/video/${ref.id}/hls/hls/`);
  });
});

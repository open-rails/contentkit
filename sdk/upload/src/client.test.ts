import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import { FakeServer, bytes, fakeClient } from "../test/fake.js";
import { UploadClient, type UploadState } from "./client.js";
import { UploadError } from "./errors.js";

const MiB = 1 << 20;
const ref = { kind: "video", id: "0192f000-0000-7000-8000-000000000001" };
const path = "source";
const STAGED = /^u-[0-9a-f-]{36}$/;

function setup(o: { retries?: number; concurrency?: number } = {}) {
  const s = new FakeServer();
  return { s, c: fakeClient(s, o) };
}

function file(n: number, seed = 1, type = "video/mp4"): File {
  return new File([bytes(n, seed)], `f${seed}.bin`, { type, lastModified: 1000 });
}

describe("single PUT", () => {
  it("hashes, presigns the path with the checksum and stages one PUT; a placed identical file is not resent", async () => {
    const { s, c } = setup();
    const f = file(3 * MiB, 2, "image/png");
    const sum = createHash("sha256").update(bytes(3 * MiB, 2)).digest("hex");
    const phases = new Set<string>();
    const up = await c.upload(f, { ref, path: "cover", onProgress: (p) => phases.add(p.phase) });
    expect(up).toMatchObject({ path: "cover.png", blob: expect.stringMatching(STAGED), type: "image/png", size: 3 * MiB, exists: false });
    expect(s.presigns[0]).toMatchObject({ path: "cover", type: "image/png", size: 3 * MiB, sha256: sum });
    expect([s.puts, [...phases]]).toEqual([[`fake://s3/put/${up.blob}`], ["hashing", "uploading"]]);
    await c.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    const again = await c.upload(f, { ref, path: "cover" });
    expect([again.exists, again.blob, s.puts.length]).toEqual([true, `sha256-${sum}`, 1]);
  });

  it("retries a dropped PUT", async () => {
    const { s, c } = setup();
    s.dropPuts = 2;
    await c.upload(file(MiB), { ref, path });
    expect(s.puts.length).toBe(3);
  });

  it("surfaces a refused presign without sending bytes", async () => {
    const { s, c } = setup();
    s.refuse = { status: 429, code: "rate_limited", error: "too many uploads", retry_after: 60 };
    const err = await c.upload(file(MiB), { ref, path }).catch((e) => e);
    expect(err).toBeInstanceOf(UploadError);
    expect([err.code, err.retryAfter, err.isLimit, s.puts.length]).toEqual(["rate_limited", 60, true, 0]);
  });

  it("puts an upload under the path the server names, and waits until it is processed", async () => {
    const { s, c } = setup();
    const phases: string[] = [];
    const f = await c.put(file(MiB, 4, "image/png"), { ref, path: "inline/x.png", edit: { rotate: 90 }, onProgress: (p) => phases.push(p.phase) });
    expect(f).toMatchObject({ path: "inline/i-1.png", upload: true, edit: { rotate: 90 } });
    expect(f.pending).toBeUndefined();
    expect(s.commits[0]).toEqual([{ op: "put", path: "inline/i-1.png", blob: expect.stringMatching(STAGED), edit: { rotate: 90 } }]);
    expect(phases.at(-1)).toBe("processing");
    expect(s.calls.filter((x) => x === "/read")).toHaveLength(1);
  });
});

describe("multipart", () => {
  const size = 70 * MiB + 123;

  it("hashes the whole file first, then stages it in parts", async () => {
    const { s, c } = setup();
    const phases: string[] = [];
    const up = await c.upload(file(size), { ref, path, onProgress: (p) => phases.at(-1) !== p.phase && phases.push(p.phase) });
    expect(phases).toEqual(["hashing", "uploading", "completing"]);
    expect(s.presigns[0]!.sha256).toBe(createHash("sha256").update(bytes(size, 1)).digest("hex"));
    expect(up).toMatchObject({ path: "source.mp4", blob: expect.stringMatching(STAGED) });
    expect(s.objects.get(up.blob)).toBe(size);
  });

  it("refuses before any part moves", async () => {
    const { s, c } = setup();
    s.refuse = { status: 413, code: "quota_exceeded", error: "over quota" };
    const err = await c.upload(file(size), { ref, path }).catch((e) => e);
    expect([err.code, s.puts.length, s.calls]).toEqual(["quota_exceeded", 0, ["/presign"]]);
  });
  it("uploads parts within the bounds, retries a dropped part and completes", async () => {
    const { s, c } = setup();
    s.dropPuts = 1;
    const states: (UploadState | null)[] = [];
    let last = 0;
    const up = await c.upload(file(size), { ref, path, onState: (st) => states.push(st), onProgress: (p) => (last = p.loaded) });
    expect(up).toMatchObject({ size, exists: false });
    expect(states.at(-1)).toBeNull();
    const plan = states.at(-2)!.parts;
    expect(plan.reduce((n, p) => n + p.size, 0)).toBe(size);
    for (const p of plan.slice(0, -1)) expect(p.size).toBeGreaterThanOrEqual(8 * MiB);
    for (const p of plan) expect(p.size).toBeLessThanOrEqual(16 * MiB);
    expect(s.puts.length).toBe(plan.length + 1);
    expect(last).toBe(size);
  });

  it("presigns parts ahead of the PUTs but never exceeds the PUT concurrency", async () => {
    const s = new FakeServer();
    let active = 0;
    let peak = 0;
    let ahead = 0;
    let started = 0;
    const c = new UploadClient({
      endpoint: "http://x/api",
      fetch: s.fetch,
      retryDelay: () => 0,
      concurrency: 2,
      transport: async (req, body, o) => {
        peak = Math.max(peak, ++active);
        started++;
        await new Promise((r) => setTimeout(r, 20));
        ahead = Math.max(ahead, s.calls.filter((p) => p === "/parts").length - started);
        try {
          await s.transport(req, body, o);
        } finally {
          active--;
        }
      },
    });
    await c.upload(file(size, 3), { ref, path });
    expect(peak).toBe(2);
    expect(ahead).toBeGreaterThan(0);
  });

  it("resumes from saved state, sending only the parts that did not land", async () => {
    const { s, c } = setup({ retries: 0, concurrency: 1 });
    const f = file(size, 5);
    let saved: UploadState | null = null;
    let landed = 0;
    const ctl = new AbortController();
    const first = c.upload(f, {
      ref,
      path,
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

    const landedParts = s.uploads.get(saved!.ticket)!.parts.size;
    expect(landedParts).toBeGreaterThan(0);
    expect(landed).toBeGreaterThanOrEqual(16 * MiB);
    const before = s.puts.length;
    let final: UploadState | null = null;
    const resumed = await new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport }).upload(f, {
      ref,
      path,
      resume: saved!,
      onState: (st) => st && (final = st),
    });
    expect(resumed).toMatchObject({ path: saved!.path, blob: saved!.blob, size });
    expect(s.puts.length - before).toBe(final!.parts.length - landedParts);
    expect(s.calls).toContain("/parts/list");
  });

  it("refuses to resume with a different file", async () => {
    const { c } = setup();
    const state = { ticket: "t", path: "source.mp4", blob: "u-x", type: "video/mp4", size, ref, file: { size, name: "other", lastModified: 1 }, limits: { minPartSize: 8 * MiB, maxPartSize: 16 * MiB, maxParts: 9 }, parts: [] };
    const err = await c.upload(file(size), { ref, path, resume: state }).catch((e) => e);
    expect(err.code).toBe("resume_mismatch");
  });

  it("stops every part when one fails for good", async () => {
    const { s, c } = setup({ retries: 1 });
    s.dropPuts = 100;
    const err = await c.upload(file(size), { ref, path }).catch((e) => e);
    expect(err.code).toBe("network");
    // Before the failing part's second PUT, other parts may take the slot
    // (how many depends on hashing speed). The deterministic bounds: at most
    // pacer concurrency (1) + 2 parts in flight, one PUT each, and the part
    // that failed for good PUT last; upload() settles every part first.
    const tries = new Map<string, number>();
    for (const u of s.puts) tries.set(u, (tries.get(u) ?? 0) + 1);
    expect(tries.size).toBeLessThanOrEqual(3);
    expect([...tries.values()].filter((n) => n === 2)).toHaveLength(1);
    expect([...tries.values()].every((n) => n <= 2)).toBe(true);
    expect(tries.get(s.puts.at(-1)!)).toBe(2);
  });
});

describe("commit", () => {
  const gallery = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };
  const put = (path: string, blob: string) => ({ op: "put" as const, path, blob });

  it("uploads a file again when its blob is due for cleanup, then commits once more", async () => {
    const { s, c } = setup();
    const a = file(1000, 11, "image/png");
    const b = file(1000, 12, "image/png");
    const ua = await c.upload(a, { ref: gallery, path: "originals/1.png" });
    const ub = await c.upload(b, { ref: gallery, path: "originals/2.png" });
    s.stale.add(ub.blob);
    const commits = () => s.calls.filter((x) => x === "/commit").length;
    const files = await c.commit(gallery, [put(ua.path, ua.blob), put(ub.path, ub.blob)], { sources: { [ua.blob]: a, [ub.blob]: b } });
    expect(files.map((f) => f.path)).toEqual(["originals/1.png", "originals/2.png"]);
    expect([commits(), s.puts.length]).toEqual([2, 3]); // only b went up again, staged anew
    const retried = s.commits.at(-1)!.map((op) => op.blob);
    expect(retried[0]).toBe(ua.blob);
    expect(retried[1]).toMatch(STAGED);
    expect(retried[1]).not.toBe(ub.blob);
  });

  it("uploads every sourced file again when the refusal names none", async () => {
    const { s, c } = setup();
    s.omitBlobs = true;
    const a = file(1000, 14, "image/png");
    const b = file(1000, 15, "image/png");
    const ua = await c.upload(a, { ref: gallery, path: "originals/1.png" });
    const ub = await c.upload(b, { ref: gallery, path: "originals/2.png" });
    s.stale.add(ub.blob);
    await c.commit(gallery, [put(ua.path, ua.blob), put(ub.path, ub.blob)], { sources: { [ua.blob]: a, [ub.blob]: b } });
    // Neither was placed, so both are staged and sent again.
    expect([s.calls.filter((x) => x === "/presign").length, s.puts.length]).toEqual([4, 4]);
  });

  it("gives up after one retry, and without sources", async () => {
    const { s, c } = setup();
    const a = file(1000, 13, "image/png");
    const ua = await c.upload(a, { ref: gallery, path: "originals/1.png" });
    s.stale.add(ua.blob);
    const ops = [put(ua.path, ua.blob)];
    expect((await c.commit(gallery, ops).catch((e) => e)).code).toBe("not_uploaded");
    s.objects.delete(ua.blob); // gone even after the re-upload attempt below
    s.transport = async () => {}; // the PUT "succeeds" but nothing lands
    const c2 = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
    const err = await c2.commit(gallery, ops, { sources: { [ua.blob]: a } }).catch((e) => e);
    expect(err.code).toBe("not_uploaded");
    expect(err.blobs).toEqual([expect.stringMatching(STAGED)]);
    expect(err.blobs[0]).not.toBe(ua.blob);
    expect(s.calls.filter((x) => x === "/commit").length).toBe(3);
  });
});

describe("reads", () => {
  const edit = { crop: { x: 400, y: 600, w: 2000, h: 2000 }, rotate: 90 };

  it("waits for an upload by path or stem, and rejects on its failure or absence", async () => {
    const { s, c } = setup();
    await c.put(file(1000, 5, "image/png"), { ref, path: "cover", wait: false });
    s.pendingReads = 2;
    await c.commit(ref, [{ op: "edit", path: "cover.png", edit }]);
    expect(await c.waitFor(ref, "cover", { interval: 1 })).toMatchObject({ path: "cover.png", edit });
    expect(s.calls.filter((x) => x === "/read")).toHaveLength(3);
    // An upload with nothing pending is not processed while it is staged.
    s.pendingReads = 0;
    s.stagedReads = 2;
    await c.commit(ref, [{ op: "edit", path: "cover.png" }]);
    expect(await c.waitFor(ref, "cover", { interval: 1 })).toMatchObject({ path: "cover.png" });
    expect(s.calls.filter((x) => x === "/read")).toHaveLength(6);
    s.seed(ref, [{ path: "source.mp4", type: "video/mp4", size: 10, staged: true }]);
    expect((await c.waitFor(ref, "source", { timeout: 0 }).catch((e) => e)).code).toBe("render_timeout");
    s.seed(ref, [{ path: "cover.png", type: "image/png", size: 10, failed: { of: "x", message: "too small", code: "image_too_small", details: { width: 100, min_width: 300 } } }]);
    const failed = await c.waitFor(ref, "cover").catch((e) => e);
    expect([failed.code, failed.details?.min_width, failed.refusal]).toEqual(["image_too_small", 300, true]);
    expect((await c.waitFor(ref, "banner").catch((e) => e)).code).toBe("not_found");
    s.seed(ref, [{ path: "poster.png", type: "image/png", frame: { t: 3 } }]);
    expect((await c.waitFor(ref, "poster", { timeout: 0 }).catch((e) => e)).code).toBe("render_timeout");
  });

  it("finds the editor view of an upload for re-cropping", async () => {
    const { s, c } = setup();
    await c.put(file(1000, 6, "image/jpeg"), { ref, path: "cover", edit });
    expect(await c.editorView(ref, "cover")).toEqual({ url: "fake://cdn/private/e-cover.jpg", width: 4000, height: 3000 });
    expect((await c.editorView(ref, "banner").catch((e) => e)).code).toBe("not_found");
    expect(s.commits[0]![0]).toMatchObject({ op: "put", path: "cover.jpg", edit });
  });

  it("reads with options as query flags, and builds HLS and frame URLs", async () => {
    const { s, c } = setup();
    const urls: string[] = [];
    const spy = new UploadClient({ endpoint: "http://x/api", readEndpoint: "http://x/read/", fetch: ((u: string, i: RequestInit) => (urls.push(String(u)), s.fetch(u, i))) as typeof fetch });
    await spy.read(ref, { prefix: "low-res/", offset: 50, limit: 25, download: true, editor: true });
    await spy.getFrame(ref, "source.mp4", 1.5, 320);
    expect(urls).toEqual([
      `http://x/read/video/${ref.id}?prefix=low-res%2F&offset=50&limit=25&download=1&editor=1`,
      `http://x/api/frame?kind=video&id=${ref.id}&path=source.mp4&t=1.5&w=320`,
    ]);
    expect(spy.hlsBase(ref, "hls/")).toBe(`http://x/read/video/${ref.id}/hls/hls/`);
    expect(() => new UploadClient({ endpoint: "/u" }).hlsBase(ref, "hls/")).toThrow(/readEndpoint/);
  });
});

it("refuses a ref whose content id is not a UUIDv7 before any request", async () => {
  const { isContentId } = await import("./ref.js");
  const { UploadApi } = await import("./api.js");
  expect(isContentId("0192f000-0000-7000-8000-000000000001")).toBe(true);
  for (const bad of ["1", "0192f000-0000-4000-8000-000000000001", "0192F000-0000-7000-8000-000000000001", "0192f000-0000-7000-c000-000000000001"]) expect(isContentId(bad)).toBe(false);
  let called = false;
  const api = new UploadApi({ endpoint: "/u", fetch: (async () => ((called = true), new Response("{}"))) as typeof fetch });
  await expect(api.presign({ ref: { kind: "post", id: "18" }, path: "cover", type: "image/png", size: 1, sha256: "0".repeat(64) })).rejects.toMatchObject({ code: "invalid_request" });
  expect(called).toBe(false);
});

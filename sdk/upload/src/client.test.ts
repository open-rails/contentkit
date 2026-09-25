import { describe, expect, it } from "vitest";
import { FakeServer, bytes } from "../test/fake.js";
import { UploadClient, type UploadState } from "./client.js";
import { UploadError } from "./errors.js";

const MiB = 1 << 20;
const ref = { kind: "video", id: "0192f000-0000-7000-8000-000000000001" };

function setup(o: { retries?: number; concurrency?: number } = {}) {
  const s = new FakeServer();
  const c = new UploadClient({
    endpoint: "http://x/api",
    fetch: s.fetch,
    transport: s.transport,
    retryDelay: () => 0,
    ...o,
  });
  return { s, c };
}

function file(n: number, seed = 1, type = "video/mp4"): File {
  return new File([bytes(n, seed)], `f${seed}.bin`, { type, lastModified: 1000 });
}

describe("single PUT", () => {
  it("hashes, presigns with the checksum and PUTs; an identical file is not resent", async () => {
    const { s, c } = setup();
    const f = file(3 * MiB, 2, "image/png");
    const phases = new Set<string>();
    const up = await c.upload(f, { ref, onProgress: (p) => phases.add(p.phase) });
    expect(up.name).toBe("sha256-" + up.sha256);
    expect([up.exists, up.size, s.puts.length]).toEqual([false, 3 * MiB, 1]);
    expect([...phases]).toEqual(["hashing", "uploading"]);
    const again = await c.upload(f, { ref });
    expect([again.exists, s.puts.length]).toEqual([true, 1]);
  });

  it("retries a dropped PUT", async () => {
    const { s, c } = setup();
    s.dropPuts = 2;
    await c.upload(file(MiB), { ref });
    expect(s.puts.length).toBe(3);
  });

  it("surfaces a refused presign without sending bytes", async () => {
    const { s, c } = setup();
    s.refuse = { status: 429, code: "rate_limited", error: "too many uploads", retry_after: 60 };
    const err = await c.upload(file(MiB), { ref }).catch((e) => e);
    expect(err).toBeInstanceOf(UploadError);
    expect([err.code, err.retryAfter, err.isLimit, s.puts.length]).toEqual(["rate_limited", 60, true, 0]);
  });

  it("uploads and commits a slot", async () => {
    const { s, c } = setup();
    const up = await c.uploadSlot(file(MiB, 3, "image/png"), { ref, slot: "cover" });
    expect(up.name).toBe("cover");
    expect(s.calls).toEqual(["/presign", "/commit-slot"]);
  });

  it("uploads and commits an inline image under the name the server picks", async () => {
    const { s, c } = setup();
    const up = await c.uploadInline(file(MiB, 4, "image/png"), { ref });
    expect(up.name).toMatch(/^i-/);
    expect(s.calls).toEqual(["/presign", "/commit-slot"]);
    expect(s.slots).toEqual([up.name]);
  });
});

describe("multipart", () => {
  const size = 70 * MiB + 123;

  it("presigns before hashing, so a refused upload reads nothing", async () => {
    const { s, c } = setup();
    s.refuse = { status: 413, code: "quota_exceeded", error: "over quota" };
    let hashed = false;
    const err = await c
      .upload(file(size), { ref, onProgress: (p) => (hashed ||= p.phase === "hashing") })
      .catch((e) => e);
    expect([err.code, hashed, s.puts.length]).toEqual(["quota_exceeded", false, 0]);
  });

  it("uploads parts within the bounds, retries a dropped part and completes", async () => {
    const { s, c } = setup();
    s.dropPuts = 1;
    const states: (UploadState | null)[] = [];
    let last = 0;
    const up = await c.upload(file(size), { ref, onState: (st) => states.push(st), onProgress: (p) => (last = p.loaded) });
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
    await c.upload(file(size, 3), { ref });
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
      signal: ctl.signal,
      onState: (st) => (saved = st),
      onProgress: (p) => {
        if (p.loaded >= 16 * MiB && !ctl.signal.aborted) {
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
      resume: saved!,
      onState: (st) => st && (final = st),
    });
    expect(resumed).toMatchObject({ name: saved!.name, size });
    expect(s.puts.length - before).toBe(final!.parts.length - landedParts);
    expect(s.calls).toContain("/parts/list");
  });

  it("refuses to resume with a different file", async () => {
    const { c } = setup();
    const state = { ticket: "t", name: "u-1", type: "video/mp4", size, ref, file: { size, name: "other", lastModified: 1 }, limits: { minPartSize: 8 * MiB, maxPartSize: 16 * MiB, maxParts: 9 }, parts: [] };
    const err = await c.upload(file(size), { ref, resume: state }).catch((e) => e);
    expect(err.code).toBe("resume_mismatch");
  });

  it("stops every part when one fails for good", async () => {
    const { s, c } = setup({ retries: 1 });
    s.dropPuts = 100;
    const err = await c.upload(file(size), { ref }).catch((e) => e);
    expect(err.code).toBe("network");
    const n = s.puts.length; // a queued part may take the slot during the failing part's backoff
    expect(n).toBeLessThanOrEqual(3);
    await new Promise((r) => setTimeout(r, 50));
    expect(s.puts.length).toBe(n);
  });
});

describe("commit", () => {
  const gallery = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "en" };

  it("uploads a file again when its original is due for cleanup, then commits once more", async () => {
    const { s, c } = setup();
    const a = file(1000, 11, "image/png");
    const b = file(1000, 12, "image/png");
    const ua = await c.upload(a, { ref: gallery });
    const ub = await c.upload(b, { ref: gallery });
    s.stale.add(ub.name);
    const commits = () => s.calls.filter((x) => x === "/commit").length;
    const files = await c.commit(
      gallery,
      [{ op: "insert", name: "1.png", original: ua.name }, { op: "insert", name: "2.png", original: ub.name }],
      { sources: { [ua.name]: a, [ub.name]: b } },
    );
    expect(files.map((f) => f.original)).toEqual([ua.name, ub.name]);
    expect([commits(), s.puts.length, s.stale.size]).toEqual([2, 3, 0]); // only b went up again
  });

  it("uploads every sourced file again when the refusal names none", async () => {
    const { s, c } = setup();
    s.omitOriginals = true;
    const a = file(1000, 14, "image/png");
    const b = file(1000, 15, "image/png");
    const ua = await c.upload(a, { ref: gallery });
    const ub = await c.upload(b, { ref: gallery });
    s.stale.add(ub.name);
    await c.commit(
      gallery,
      [{ op: "insert", name: "1.png", original: ua.name }, { op: "insert", name: "2.png", original: ub.name }],
      { sources: { [ua.name]: a, [ub.name]: b } },
    );
    // Both went through presign again; only the stale one needed bytes.
    expect([s.calls.filter((x) => x === "/presign").length, s.puts.length]).toEqual([4, 3]);
  });

  it("gives up after one retry, and without sources", async () => {
    const { s, c } = setup();
    const a = file(1000, 13, "image/png");
    const ua = await c.upload(a, { ref: gallery });
    s.stale.add(ua.name);
    const ops = [{ op: "insert" as const, name: "1.png", original: ua.name }];
    expect((await c.commit(gallery, ops).catch((e) => e)).code).toBe("not_uploaded");
    s.objects.delete(ua.name); // gone even after the re-upload attempt below
    const t = s.transport;
    s.transport = async () => {}; // the PUT "succeeds" but nothing lands
    const c2 = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
    expect((await c2.commit(gallery, ops, { sources: { [ua.name]: a } }).catch((e) => e)).code).toBe("not_uploaded");
    expect(s.calls.filter((x) => x === "/commit").length).toBe(3);
    s.transport = t;
  });
});

describe("slots", () => {
  const edit = { crop: { x: 400, y: 600, w: 2000, h: 2000 }, rotate: 90 };

  it("commits the crop with the original and returns the manifest", async () => {
    const { s, c } = setup();
    const up = await c.uploadSlot(file(1000, 4, "image/jpeg"), { ref, slot: "avatar", edit });
    expect(s.slotCalls).toEqual([{ ref, slot: "avatar", sha256: up.sha256, edit }]);
    expect(up.manifest).toMatchObject({ aspect: "1:1", edit, dims: { w: 4000, h: 3000 }, outputs: [{ w: 128 }, { w: 256 }, { w: 512 }] });
    expect(s.calls).toEqual(["/presign", "/commit-slot"]);
  });

  it("omits the edit when not given", async () => {
    const { s, c } = setup();
    await c.uploadSlot(file(1000, 5, "image/jpeg"), { ref, slot: "cover" });
    expect(s.slotCalls[0]).not.toHaveProperty("edit");
  });

  it("re-crops without uploading and reads the manifest", async () => {
    const { s, c } = setup();
    expect((await c.getSlot(ref, "cover")).outputs).toEqual([]);
    await c.uploadSlot(file(1000, 6, "image/jpeg"), { ref, slot: "cover" });
    const puts = s.puts.length;
    const m = await c.editSlot(ref, "cover", edit);
    expect([m.edit, s.puts.length]).toEqual([edit, puts]);
    expect(await c.getSlot(ref, "cover")).toEqual(m);
    expect((await c.getSlotOriginal(ref, "cover")).size).toBe(64);
    expect((await c.editSlot(ref, "banner", edit).catch((e) => e)).code).toBe("not_found");
    expect((await c.getSlotOriginal(ref, "banner").catch((e) => e)).code).toBe("not_found");
  });

  it("waits for the outputs to be encoded", async () => {
    const { s, c } = setup();
    s.pendingReads = 2;
    const up = await c.uploadSlot(file(1000, 7, "image/jpeg"), { ref, slot: "avatar" });
    expect(up.manifest.pending).toBe(true);
    const m = await c.waitForSlot(ref, "avatar", { interval: 1 });
    expect(m.pending).toBe(false);
    expect(s.calls.filter((p) => p === "/slot")).toHaveLength(3);
  });
});

it("refuses a ref whose content id is not a UUIDv7 before any request", async () => {
  const { isContentId } = await import("./ref.js");
  const { UploadApi } = await import("./api.js");
  expect(isContentId("0192f000-0000-7000-8000-000000000001")).toBe(true);
  for (const bad of ["1", "0192f000-0000-4000-8000-000000000001", "0192F000-0000-7000-8000-000000000001", "0192f000-0000-7000-c000-000000000001"]) expect(isContentId(bad)).toBe(false);
  let called = false;
  const api = new UploadApi({ endpoint: "/u", fetch: (async () => ((called = true), new Response("{}"))) as typeof fetch });
  await expect(api.presign({ ref: { kind: "post", id: "18" }, type: "image/png", size: 1 })).rejects.toMatchObject({ code: "invalid_request" });
  expect(called).toBe(false);
});

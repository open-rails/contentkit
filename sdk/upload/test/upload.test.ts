import type { ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { UploadClient, UploadQueue, fetchTransport, type QueueSnapshot, type Transport, type UploadState } from "../src/index.js";
import { bytes } from "./fake.js";
import { KillProxy, startServer, stopServer } from "./server.js";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
const MiB = 1 << 20;

describe.skipIf(!endpoint)("upload against MinIO and media.UploadHandler", () => {
  let proxy: KillProxy;
  let proc: ChildProcess;
  let base: string;

  beforeAll(async () => {
    proxy = new KillProxy(new URL(endpoint!));
    const s = await startServer(await proxy.listen());
    proc = s.proc;
    base = s.url;
  });

  afterAll(async () => {
    if (proc) await stopServer(proc);
    await proxy?.close();
  });

  const client = (o: { retries?: number; transport?: Transport } = {}) =>
    new UploadClient({
      endpoint: `${base}/upload`,
      headers: () => ({ "X-Test-Actor": "alice" }),
      retryDelay: () => 200,
      ...o,
    });

  async function stored(ref: { kind: string; id: string; version?: string }, name: string, key = "name") {
    const q = new URLSearchParams({ ...ref, [key]: name });
    return (await (await fetch(`${base}/object?${q}`)).json()) as { size: number; sha256: string; edit?: string };
  }

  it("uploads a small image and commits it", async () => {
    const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "en" };
    const body = bytes(4096, 7);
    const c = client();
    const up = await c.upload(new File([body], "a.png", { type: "image/png" }), { ref });
    expect(up.name).toBe("sha256-" + createHash("sha256").update(body).digest("hex"));
    const files = await c.commit(ref, [{ op: "insert", name: "001.png", original: up.name }]);
    expect(files).toMatchObject([{ name: "001.png", original: up.name, type: "image/png", size: 4096 }]);
    expect((await c.upload(new File([body], "a.png", { type: "image/png" }), { ref })).exists).toBe(true);

    const refused = await c
      .upload(new File([body], "a.gif", { type: "image/gif" }), { ref })
      .catch((e) => e);
    expect([refused.code, refused.status]).toEqual(["type_not_allowed", 415]);
  });

  it("uploads an inline image under a server-chosen name and commits it", async () => {
    const ref = { kind: "post", id: "0192f000-0000-7000-8000-000000000003" };
    const body = bytes(2048, 9);
    const up = await client().uploadInline(new File([body], "i.png", { type: "image/png" }), { ref });
    expect(up.name).toMatch(/^i-[0-9a-f-]{36}$/);
    expect(up.url).toBe(`http://media.invalid/sdk/post/${ref.id}/public/sha256-${createHash("sha256").update(body).digest("hex")}`);
    expect(await stored(ref, up.name)).toMatchObject({ size: 2048, sha256: createHash("sha256").update(body).digest("hex") });
  });

  it("edits a file, sets a slot from it and refuses files over the kind's cap", async () => {
    const ref = { kind: "post", id: "0192f000-0000-7000-8000-000000000001" };
    const c = client();
    const page = bytes(3000, 11);
    const a = await c.upload(new File([page], "a.png", { type: "image/png" }), { ref });
    const b = await c.upload(new File([bytes(3000, 12)], "b.png", { type: "image/png" }), { ref });
    await c.commit(ref, [
      { op: "insert", name: "a.png", original: a.name },
      { op: "insert", name: "b.png", original: b.name },
    ]);

    const files = await c.edit(ref, "a.png", { crop: { x: 10, y: 20, w: 30, h: 40 }, rotate: 90 });
    expect(files[0]).toMatchObject({ name: "a.png", edit: { crop: { x: 10, y: 20, w: 30, h: 40 }, rotate: 90 } });
    expect((await c.edit(ref, "a.png", { rotate: 45 as 90 }).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.edit(ref, "a.png", null))[0]!.edit).toBeUndefined();

    // The slot's 1:2 aspect sets the crop's height.
    await c.setSlotFromFile(ref, "cover", "a.png", { crop: { x: 5, y: 0, w: 50, h: 0 } });
    const slot = await stored(ref, "cover", "slot");
    expect(slot.sha256).toBe(createHash("sha256").update(page).digest("hex"));
    expect(JSON.parse(slot.edit!)).toEqual({ crop: { x: 5, y: 0, w: 50, h: 100 } });

    const c3 = await c.upload(new File([bytes(3000, 13)], "c.png", { type: "image/png" }), { ref });
    const refused = await c.commit(ref, [{ op: "insert", name: "c.png", original: c3.name }]).catch((e) => e);
    expect([refused.code, refused.status, refused.isCeiling]).toEqual(["too_many_files", 409, true]);
  });

  it("uploads a slot original with an edit, re-edits it and serves the original back", async () => {
    // 360×240; the cover slot is 3:1, so a 360-wide crop is 120 high.
    const png = readFileSync(new URL("../e2e/fixtures/small.png", import.meta.url));
    const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000004", version: "en" };
    const c = client();
    const puts: string[] = [];
    const counting: Transport = async (req, blob, o) => {
      puts.push(req.url);
      await fetchTransport(req, blob, o);
    };
    const edit = { crop: { x: 0, y: 40, w: 360, h: 120 } };
    const up = await client({ transport: counting }).uploadSlot(new File([png], "cover.png", { type: "image/png" }), { ref, slot: "cover", edit });
    // This server has no encoder: the slot stays pending, and dims arrive with the first encode.
    expect(up.manifest).toMatchObject({ aspect: "3:1", edit, pending: true, outputs: [] });
    expect(puts).toHaveLength(1);
    expect(JSON.parse((await stored(ref, "cover", "slot")).edit!)).toEqual(edit);

    // Re-edit from the kept original: nothing is uploaded.
    const turned = { crop: { x: 100, y: 0, w: 80, h: 240 }, rotate: 90 };
    const m = await c.editSlot(ref, "cover", turned);
    expect(m.edit).toEqual(turned);
    expect(await c.getSlot(ref, "cover")).toMatchObject({ aspect: "3:1", edit: turned, pending: true });
    expect(puts).toHaveLength(1);

    const original = new Uint8Array(await (await c.getSlotOriginal(ref, "cover")).arrayBuffer());
    expect(createHash("sha256").update(original).digest("hex")).toBe(createHash("sha256").update(png).digest("hex"));

    expect((await c.editSlot(ref, "cover", { rotate: 45 }).catch((e) => e)).code).toBe("invalid_request");
    expect((await c.editSlot(ref, "banner", edit).catch((e) => e)).code).toBe("not_found");
    expect((await c.getSlotOriginal({ ...ref, id: "0192f000-0000-7000-8000-000000000005" }, "cover").catch((e) => e)).code).toBe("not_found");
  });

  it("uploads a stale original again when commit refuses it", async () => {
    // The server's sweep grace is 20 s: commit refuses an unreferenced original after 15 s,
    // and accepts a fresh one for 14 s (presign reuses one for 9 s).
    const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000002", version: "en" };
    const body = bytes(2048, 8);
    const file = new File([body], "b.png", { type: "image/png" });
    const c = client();
    const up = await c.upload(file, { ref });
    await new Promise((r) => setTimeout(r, 17_000));
    const ops = [{ op: "insert" as const, name: "001.png", original: up.name }];
    expect((await c.commit(ref, ops).catch((e) => e)).code).toBe("not_uploaded");
    const files = await c.commit(ref, ops, { sources: { [up.name]: file } });
    expect(files).toMatchObject([{ name: "001.png", original: up.name, size: 2048 }]);
  });

  it("survives a killed connection mid-part, resumes and commits a >64 MiB file", async () => {
    const ref = { kind: "video", id: "0192f000-0000-7000-8000-000000000001" };
    const body = bytes(72 * MiB + 4321, 9);
    const file = new File([body], "v.mp4", { type: "video/mp4", lastModified: 1 });

    // Session 1: no retries; part 3's connection drops mid-body.
    let saved: UploadState | null = null;
    proxy.kill(3);
    const first = await client({ retries: 0 })
      .upload(file, { ref, onState: (s) => (saved = s) })
      .catch((e) => e);
    expect(first.code).toBe("network");
    expect(proxy.kills).toBe(1);
    expect(saved).not.toBeNull();

    // Session 2 (a reload): resume from the saved state; another drop is retried in place.
    let parts = 0;
    const states: UploadState[] = [];
    const counting: Transport = async (req, blob, o) => {
      await fetchTransport(req, blob, o);
      parts++;
    };
    proxy.kill("next");
    const c = client({ transport: counting });
    const up = await c.upload(file, { ref, resume: saved!, onState: (s) => s && states.push(s) });
    expect(proxy.kills).toBe(2);
    expect(up).toMatchObject({ name: saved!.name, size: body.length, type: "video/mp4" });
    const landed = states.at(-1)!.parts.length - parts;
    expect(landed).toBeGreaterThanOrEqual(1);

    const files = await c.commit(ref, [{ op: "insert", name: "video.mp4", original: up.name }]);
    expect(files).toMatchObject([{ name: "video.mp4", size: body.length }]);
    expect(await stored(ref, up.name)).toEqual({
      size: body.length,
      sha256: createHash("sha256").update(body).digest("hex"),
    });
  });
  it("processes on upload: stages files unattached, attaches them in queue order, discards a removed one", async () => {
    const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000006", version: "en" };
    const c = new UploadClient({ endpoint: `${base}/upload-on-upload`, headers: () => ({ "X-Test-Actor": "alice" }), retryDelay: () => 200 });
    const q = new UploadQueue(c, { ref, pollInterval: 100 });
    const until = (ok: (s: QueueSnapshot) => boolean) =>
      new Promise<QueueSnapshot>((resolve) => {
        const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
        const off = q.subscribe(check);
        check();
      });
    const png = (name: string, seed: number) => new File([bytes(3000, seed)], name, { type: "image/png" });
    const [, b, d] = q.add([png("a.png", 21), png("b.png", 22), png("d.png", 23)]);
    const staged = await until((x) => x.items.every((i) => i.unattached));
    expect((await c.files(ref)).map((f) => [f.name, f.unattached]).sort()).toEqual([
      ["a.png", true],
      ["b.png", true],
      ["d.png", true],
    ]);

    const discarded = staged.items.find((i) => i.id === d!.id)!.result!.name;
    q.remove(d!.id);
    for (let i = 0; (await c.files(ref)).some((f) => f.name === "d.png"); i++) {
      if (i > 50) throw new Error("d.png was not discarded");
      await new Promise((r) => setTimeout(r, 100));
    }
    const q2 = new URLSearchParams({ ...ref, name: discarded });
    expect((await fetch(`${base}/object?${q2}`)).status).toBe(404);

    q.move(b!.id, 0);
    const files = await q.commit();
    expect(files.map((f) => f.name)).toEqual(["b.png", "a.png"]);
    expect((await c.files(ref)).map((f) => [f.name, !!f.unattached])).toEqual([
      ["b.png", false],
      ["a.png", false],
    ]);
    expect(q.getSnapshot().items.every((i) => i.status === "committed")).toBe(true);
    q.dispose();
  });
});

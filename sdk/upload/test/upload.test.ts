import type { ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { UploadClient, fetchTransport, type Transport, type UploadState } from "../src/index.js";
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

  async function stored(ref: { kind: string; id: string; version?: string }, name: string) {
    const q = new URLSearchParams({ ...ref, name });
    return (await (await fetch(`${base}/object?${q}`)).json()) as { size: number; sha256: string };
  }

  it("uploads a small image and commits it", async () => {
    const ref = { kind: "gallery", id: "1", version: "en" };
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

  it("uploads a stale original again when commit refuses it", async () => {
    // The server's sweep grace is 8 s: commit accepts an unreferenced original for ~6 s.
    const ref = { kind: "gallery", id: "2", version: "en" };
    const body = bytes(2048, 8);
    const file = new File([body], "b.png", { type: "image/png" });
    const c = client();
    const up = await c.upload(file, { ref });
    await new Promise((r) => setTimeout(r, 9000));
    const ops = [{ op: "insert" as const, name: "001.png", original: up.name }];
    expect((await c.commit(ref, ops).catch((e) => e)).code).toBe("not_uploaded");
    const files = await c.commit(ref, ops, { sources: { [up.name]: file } });
    expect(files).toMatchObject([{ name: "001.png", original: up.name, size: 2048 }]);
  });

  it("survives a killed connection mid-part, resumes and commits a >64 MiB file", async () => {
    const ref = { kind: "video", id: "1" };
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
});

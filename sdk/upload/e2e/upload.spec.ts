import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { expect, test } from "@playwright/test";
import { KillProxy, startServer, stopServer } from "../test/server.js";
import type { ChildProcess } from "node:child_process";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
const MiB = 1 << 20;
const size = 72 * MiB + 4321;
const ref = { kind: "video", id: "0192f000-0000-7000-8000-000000000001" };
const imageRef = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000002", version: "en" };
const image = Array.from(readFileSync(resolve(import.meta.dirname, "fixtures/small.png")));
const modulePath = `/@fs/${resolve(import.meta.dirname, "../dist/index.js")}`;
const demoPort = Number(process.env.DEMO_PORT ?? 4179);

test.describe("browser uploads", () => {
  test.skip(!endpoint, "requires CONTENTKIT_TEST_S3_ENDPOINT");
  test.skip(({ isMobile }) => isMobile, "one Chromium browser run covers the upload contract");
  test.setTimeout(180_000);

  let proxy: KillProxy;
  let proc: ChildProcess;
  let origin: string;

  test.beforeAll(async () => {
    proxy = new KillProxy(new URL(endpoint!));
    origin = await proxy.listen();
    const server = await startServer(origin);
    proc = server.proc;
    proxy.serveBrowser(new URL(`http://127.0.0.1:${demoPort}`), new URL(server.url));
  });

  test.afterAll(async () => {
    if (proc) await stopServer(proc);
    await proxy?.close();
  });

  test("uploads and commits a small image", async ({ page }) => {
    await page.goto(origin);
    const result = await page.evaluate(async ({ modulePath, imageRef, image }) => {
      const { UploadClient } = (await import(/* @vite-ignore */ modulePath)) as typeof import("../src/index.js");
      const file = new File([new Uint8Array(image)], "small.png", { type: "image/png" });
      const client = new UploadClient({ endpoint: "/upload", headers: () => ({ "X-Test-Actor": "alice" }) });
      const uploaded = await client.upload(file, { ref: imageRef });
      const committed = await client.commit(imageRef, [{ op: "insert", name: "001.png", original: uploaded.name }]);
      return { uploaded, committed };
    }, { modulePath, imageRef, image });

    expect(result.committed).toEqual([expect.objectContaining({ name: "001.png", original: result.uploaded.name, size: image.length })]);
    const object = await (await page.request.get(`${origin}/object?kind=gallery&id=${imageRef.id}&version=en&name=${result.uploaded.name}`)).json();
    expect(object).toEqual({ size: image.length, sha256: createHash("sha256").update(Buffer.from(image)).digest("hex") });
  });

  test("resumes after a dropped part and commits the file once", async ({ page }) => {
    await page.goto(origin);
    // Chromium can replay a reset PUT before surfacing an error to the SDK.
    proxy.kill(3, Number.POSITIVE_INFINITY);

    const first = await page.evaluate(async ({ modulePath, ref, size }) => {
      const { UploadClient } = (await import(/* @vite-ignore */ modulePath)) as typeof import("../src/index.js");
      const file = new File([new Uint8Array(size).fill(9)], "video.mp4", { type: "video/mp4", lastModified: 1 });
      const client = new UploadClient({ endpoint: "/upload", headers: () => ({ "X-Test-Actor": "alice" }), retries: 0 });
      try {
        await client.upload(file, {
          ref,
          onState: (state) => state && sessionStorage.setItem("multipart", JSON.stringify(state)),
        });
        return "completed";
      } catch (error) {
        return (error as { code: string }).code;
      }
    }, { modulePath, ref, size });

    expect(proxy.kills).toBeGreaterThanOrEqual(1);
    expect(first).toBe("network");
    proxy.stopDropping();
    expect(await page.evaluate(() => sessionStorage.getItem("multipart"))).not.toBeNull();

    await page.reload();
    const result = await page.evaluate(async ({ modulePath, ref, size }) => {
      const { UploadClient } = (await import(/* @vite-ignore */ modulePath)) as typeof import("../src/index.js");
      const file = new File([new Uint8Array(size).fill(9)], "video.mp4", { type: "video/mp4", lastModified: 1 });
      const saved = JSON.parse(sessionStorage.getItem("multipart")!) as import("../src/index.js").UploadState;
      const client = new UploadClient({ endpoint: "/upload", headers: () => ({ "X-Test-Actor": "alice" }), retryDelay: () => 200 });
      const landed = (await client.api.listParts({ ticket: saved.ticket })).parts.length;
      const uploaded = await client.upload(file, {
        ref,
        resume: saved,
        onState: (state) => state
          ? sessionStorage.setItem("multipart", JSON.stringify(state))
          : sessionStorage.removeItem("multipart"),
      });
      const committed = await client.commit(ref, [{ op: "insert", name: "video.mp4", original: uploaded.name }]);
      return { uploaded, committed, landed, savedName: saved.name, storedState: sessionStorage.getItem("multipart") };
    }, { modulePath, ref, size });

    expect(result.uploaded.name).toBe(result.savedName);
    expect(result.landed).toBeGreaterThan(0);
    expect(result.committed).toEqual([expect.objectContaining({ name: "video.mp4", size })]);
    expect(result.storedState).toBeNull();

    const object = await (await page.request.get(`${origin}/object?kind=video&id=${ref.id}&name=${result.uploaded.name}`)).json();
    expect(object).toEqual({ size, sha256: createHash("sha256").update(Buffer.alloc(size, 9)).digest("hex") });
  });
});

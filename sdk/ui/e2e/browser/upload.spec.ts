import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { expect, test } from "@playwright/test";
import type { UploadState } from "@openrails/contentkit-ui/client";
import { harness, page as at } from "./pages.ts";

const MiB = 1 << 20;
const size = 72 * MiB + 4321;
const image = Array.from(readFileSync(resolve(import.meta.dirname, "../fixtures/small.png")));
const h = harness();

test.describe("browser uploads", () => {
  test.skip(({ isMobile }) => isMobile, "one Chromium browser run covers the upload contract");
  test.setTimeout(180_000);

  test("uploads a small image, puts it and waits until it is processed", async ({ page }) => {
    const owner = await h.user();
    const ref = await h.item({ kind: "gallery", owner: owner.id });
    await page.goto(at("upload.html", owner, {}));
    await expect(page.locator("body[data-ready]")).toBeAttached();
    const result = await page.evaluate(async ({ ref, image }) => {
      const { auth, createContentKitClient } = window.demo;
      const file = new File([new Uint8Array(image)], "small.png", { type: "image/png" });
      const client = createContentKitClient({ baseUrl: "/api/contentkit", fetch: auth.authFetch }).media;
      return client.put(file, { ref: { kind: ref.kind, id: ref.id }, path: "originals/001.png" });
    }, { ref, image });

    expect(result).toMatchObject({ path: "originals/001.png", size: image.length, w: 360, h: 240 });
    expect(await h.object(ref, "originals/001.png")).toEqual({ size: image.length, sha256: createHash("sha256").update(Buffer.from(image)).digest("hex") });
  });

  test("resumes after a dropped part and commits the file once", async ({ page }) => {
    const owner = await h.user();
    const it = await h.item({ kind: "file", owner: owner.id });
    const ref = { kind: it.kind, id: it.id };
    await page.goto(at("upload.html", owner, {}));
    await expect(page.locator("body[data-ready]")).toBeAttached();
    // The first part PUT lands; every later one drops (Chromium can replay a reset PUT before surfacing an error).
    await h.faults([{ item: ref.id, fault: "drop", skip: 1, after: 2 * MiB }]);

    const first = await page.evaluate(async ({ ref, size }) => {
      const { auth, createContentKitClient } = window.demo;
      const file = new File([new Uint8Array(size).fill(9)], "video.bin", { type: "application/octet-stream", lastModified: 1 });
      // Land earlier parts before interruption, regardless of backend latency.
      const client = createContentKitClient({ baseUrl: "/api/contentkit", fetch: auth.authFetch, media: { retries: 0, concurrency: 1 } }).media;
      try {
        await client.upload(file, { ref, path: "file", onState: (state) => state && sessionStorage.setItem("multipart", JSON.stringify(state)) });
        return "completed";
      } catch (error) {
        return (error as { code: string }).code;
      }
    }, { ref, size });

    expect((await h.listFaults()).find((f) => f.item === ref.id)?.hits).toBeGreaterThanOrEqual(1);
    expect(first).toBe("network");
    await h.clearFaults(ref.id);
    expect(await page.evaluate(() => sessionStorage.getItem("multipart"))).not.toBeNull();

    await page.reload();
    await expect(page.locator("body[data-ready]")).toBeAttached();
    const result = await page.evaluate(async ({ ref, size }) => {
      const { auth, createContentKitClient } = window.demo;
      const file = new File([new Uint8Array(size).fill(9)], "video.bin", { type: "application/octet-stream", lastModified: 1 });
      const saved = JSON.parse(sessionStorage.getItem("multipart")!) as UploadState;
      const client = createContentKitClient({ baseUrl: "/api/contentkit", fetch: auth.authFetch, media: { retryDelay: () => 200 } }).media;
      const landed = (await client.api.listParts(ref, { ticket: saved.ticket })).parts.length;
      const uploaded = await client.upload(file, {
        ref,
        path: "file",
        resume: saved,
        onState: (state) => (state ? sessionStorage.setItem("multipart", JSON.stringify(state)) : sessionStorage.removeItem("multipart")),
      });
      const committed = await client.commit(ref, [{ op: "put", path: uploaded.path, blob: uploaded.blob }]);
      return { uploaded, committed, landed, saved, storedState: sessionStorage.getItem("multipart") };
    }, { ref, size });

    // The parts land at a staged name; the worker places it at the hash of its bytes.
    const staged = /^u-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
    expect(result.saved).toMatchObject({ path: "file.bin", blob: expect.stringMatching(staged) });
    expect(result.uploaded).toMatchObject({ path: "file.bin", blob: result.saved.blob });
    expect(result.landed).toBeGreaterThan(0);
    expect(result.committed).toEqual([expect.objectContaining({ path: "file.bin", size })]);
    expect(result.storedState).toBeNull();

    await expect.poll(() => h.object(ref, "file"), { timeout: 60_000 }).toEqual({ size, sha256: createHash("sha256").update(Buffer.alloc(size, 9)).digest("hex") });
  });
});

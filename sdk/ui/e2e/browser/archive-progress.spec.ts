import { writeFileSync } from "node:fs";
import { expect, test } from "@playwright/test";
import { pageArchive } from "../support/archive.ts";
import { contentKit } from "../support/seed.ts";
import { admin, harness } from "./pages.ts";

test("ZIP drop shows transfer bytes, real processing previews, and reopens a durable draft", async ({ page, isMobile }, info) => {
  test.skip(isMobile, "one Chromium journey covers the ZIP processing stream");
  test.setTimeout(240_000);
  const h = harness(), owner = await h.user({ role: "staff" });
  const client = contentKit(h, owner);
  const archive = pageArchive();
  const filename = info.outputPath("Progress_25_pages.zip");
  writeFileSync(filename, archive);
  let id = "", version = "";
  // Pause the ninth real extracted source PUT. The first eight page attachments
  // and their processor facts are committed, and WebP thumbnails may still finish.
  await page.route("**/v1/contents", async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    const response = await route.fetch();
    const content = await response.json();
    id = content.id; version = content.versions[0].id;
    await h.faults([{ item: version, fault: "hold", from: "worker", path: "/private/", method: "PUT", content_type: "image/png", skip: 8 }]);
    await route.fulfill({ response });
  });
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 5, downloadThroughput: -1, uploadThroughput: 2 * 1024 * 1024 });
  try {
    await page.goto(admin("/admin/content-upload", owner, { mode: "alone", params: { kind: "comic", format: "manga", contentLanguage: "en" } }));
    const root = page.locator('[data-ckui="content-upload"]');
    await expect(root.getByRole("button", { name: "Add files" })).toBeVisible({ timeout: 30_000 });
    await page.evaluate(() => { const input = document.createElement("input"); input.id = "archive-drop-file"; input.type = "file"; input.hidden = true; document.body.append(input); });
    await page.locator("#archive-drop-file").setInputFiles(filename);
    await page.evaluate(() => {
      const input = document.querySelector<HTMLInputElement>("#archive-drop-file")!;
      const dataTransfer = new DataTransfer(); dataTransfer.items.add(input.files![0]!);
      document.querySelector('[data-ckui="upload-drop"]')!.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer }));
      input.remove();
    });
    const transfer = root.locator('[data-ckui="upload-transfer"][data-phase="uploading"]');
    await expect(transfer).toBeVisible({ timeout: 60_000 });
    await expect.poll(async () => Number(await transfer.getAttribute("data-loaded")), { timeout: 30_000 }).toBeGreaterThan(0);
    expect(Number(await transfer.getAttribute("data-loaded"))).toBeLessThan(archive.length);
    await expect(transfer).toContainText(/MB\s*\/\s*[\d.]+ MB/);
    await page.screenshot({ path: info.outputPath("zip-transfer.png"), fullPage: true });
    await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    const extraction = root.locator('[data-stage="archive-pages"]');
    await expect(extraction).toContainText("8/25", { timeout: 90_000 });
    const previews = root.locator('[data-ckui="media-progress-preview"][data-state="ready"] img');
    await expect(previews.first()).toBeVisible({ timeout: 90_000 });
    await expect.poll(() => previews.evaluateAll((images) => images.filter((image) => (image as HTMLImageElement).naturalWidth > 0).length)).toBeGreaterThan(0);
    await expect(root.locator('[data-ckui="media-processing"]')).toHaveAttribute("data-state", "processing");
    await expect(root.locator('[data-stage="preset:thumb"]')).toContainText(/thumb: [1-8]\/8/);
    expect((await client.contents.get(id)).state).toBe("draft");
    await page.screenshot({ path: info.outputPath("zip-processing-previews.png"), fullPage: true });

    await root.getByRole("button", { name: "Open", exact: true }).click();
    await page.reload(); // no upload queue or component memory survives
    const editor = page.locator('[data-ckui="reader-content-editor"]');
    await expect(editor.locator('[data-stage="archive-pages"]')).toContainText("8/25", { timeout: 30_000 });
    await expect(editor.locator('[data-ckui="thumb"] img').first()).toBeVisible({ timeout: 30_000 });
    await h.clearFaults(version);
    await expect(editor.locator('[data-stage="archive-pages"]')).toHaveCount(0, { timeout: 90_000 });
    const finished = await client.media.read({ kind: "comic", id, version }, { status: true });
    expect(finished.status?.archives.pages).toMatchObject({ completed: 25, total: 25 });
    expect(finished.status?.presets.find((preset) => preset.name === "thumb")).toMatchObject({ completed: 25, total: 25 });
    expect((await client.contents.get(id)).state).toBe("draft");
    await page.screenshot({ path: info.outputPath("zip-draft-ready.png"), fullPage: true });
  } finally {
    if (version) await h.clearFaults(version);
    await cdp.detach();
  }
});

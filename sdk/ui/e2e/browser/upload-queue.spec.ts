import { createHash } from "node:crypto";
import { utimesSync, writeFileSync } from "node:fs";
import path from "node:path";
import { expect, test } from "@playwright/test";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// UploadQueue in demo/admin.tsx: every file into a new item (createItem),
// against the real upload API.
test.describe.configure({ timeout: 180_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const fixtures = path.resolve(import.meta.dirname, "..", "fixtures");

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`upload queue: files into new items (${mode}, ${theme})`, async ({ page }, info) => {
      const owner = await h.user();
      await page.goto(admin("/admin/uploads", owner, { mode, theme, params: { kind: "gallery", path: "cover" } }));
      const queue = page.locator("[data-ckui=upload-queue]");
      await expect(queue.getByRole("button", { name: "Add files" })).toBeVisible({ timeout: 30_000 });
      expect(await inheritsHostTheme(page, "upload-queue")).toBe(true);
      await page.evaluate(() => localStorage.setItem("demo.created", "0"));
      await queue.getByLabel("Add files").setInputFiles([path.join(fixtures, "small.png"), path.join(fixtures, "cover.jpg")]);
      const entries = queue.locator("[data-ckui=upload-entry]");
      await expect(entries).toHaveCount(2);
      await expect(entries.filter({ hasText: "small.png" })).toHaveAttribute("data-status", "done", { timeout: 60_000 });
      await expect(entries.filter({ hasText: "cover.jpg" })).toHaveAttribute("data-status", "done", { timeout: 60_000 });
      await expect(queue.getByText("2 of 2 uploaded")).toBeVisible();
      await expect(page.locator("[data-demo=created]")).toHaveText("2");
      await page.screenshot({ path: `${dir}/upload-queue-${mode}-${theme}-${info.project.name}.png`, fullPage: true });
    });
  }
}

test.describe("resume", () => {
  test.skip(({ isMobile }) => isMobile, "one Chromium run covers multipart resume");

  test("upload queue: a paused upload resumes after a reload, into the same item", async ({ page }, info) => {
    const owner = await h.user();
    const size = 72 * (1 << 20) + 777;
    const file = info.outputPath("big.bin");
    writeFileSync(file, Buffer.alloc(size, 7));
    utimesSync(file, new Date(1_700_000_000_000), new Date(1_700_000_000_000));
    await page.goto(admin("/admin/uploads", owner, { mode: "alone", params: { kind: "file", path: "file", hold: "1" } }));
    const queue = page.locator("[data-ckui=upload-queue]");
    await expect(queue.getByRole("button", { name: "Add files" })).toBeVisible({ timeout: 30_000 });
    await page.evaluate(() => localStorage.setItem("demo.created", "0"));
    await queue.getByLabel("Add files").setInputFiles(file);
    const entry = queue.locator("[data-ckui=upload-entry]");
    // Two parts land; the third is held: pause there.
    await expect(entry.getByRole("progressbar")).toHaveAttribute("aria-valuenow", /^[1-9][0-9]?$/, { timeout: 60_000 });
    await entry.getByRole("button", { name: "Pause" }).click();
    await expect(entry).toHaveAttribute("data-status", "paused");
    const resumeKey = await page.evaluate(() => Object.keys(localStorage).find((key) => key.startsWith("ckui.uploads:")));
    expect(resumeKey).toBeDefined();
    const id = await page.evaluate(() => localStorage.getItem("demo.last")!);
    const version = await page.evaluate(() => localStorage.getItem("demo.version")!);
    await h.clearFaults(id);

    // Leaving with an unfinished upload: after the reload it is listed, and the same file continues it.
    await page.reload();
    await expect(queue.locator("[data-ckui=upload-waiting]")).toContainText("big.bin", { timeout: 30_000 });
    await queue.getByLabel("Add files").setInputFiles(file);
    await expect(entry).toContainText("Resumed");
    await expect(entry).toHaveAttribute("data-status", "done", { timeout: 120_000 });
    await expect(page.locator("[data-demo=created]")).toHaveText("1");
    expect(await page.evaluate((key) => localStorage.getItem(key!), resumeKey)).toBeNull();
    await expect
      .poll(() => h.object({ kind: "file", id, version }, "file"), { timeout: 60_000 })
      .toEqual({ size, sha256: createHash("sha256").update(Buffer.alloc(size, 7)).digest("hex") });
  });
});

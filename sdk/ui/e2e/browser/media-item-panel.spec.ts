import path from "node:path";
import { expect, test } from "@playwright/test";
import { fixture, mediaClient, seedVideo } from "../support/seed.ts";
import { mediaRef, admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// MediaItemPanel in demo/admin.tsx over the real media worker: a video's
// state, source, subtitles, poster and regenerate.
test.describe.configure({ timeout: 240_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const fixtures = path.resolve(import.meta.dirname, "..", "fixtures");
const state = (page: import("@playwright/test").Page) => page.locator("[data-ckui=media-item-panel] [data-ckui=media-state]");

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`media item panel: state, subtitles and poster (${mode}, ${theme})`, async ({ page }, info) => {
      const owner = await h.user();
      const video = await seedVideo(h, owner);
      await page.goto(admin(`/admin/media/video/${video.id}/${video.version}`, owner, { mode, theme }));
      const panel = page.locator("[data-ckui=media-item-panel]");
      await expect(state(page)).toHaveAttribute("data-state", "ready", { timeout: 30_000 });
      expect(await inheritsHostTheme(page, "media-item-panel")).toBe(true);
      await expect(panel.locator("[data-ckui=media-source]")).toContainText("0:12");

      // Subtitles: a full track and a forced one, then one removed.
      const subs = panel.locator("[data-ckui=media-subtitles]");
      await subs.getByLabel("Subtitle file (.vtt, .srt)").setInputFiles(path.join(fixtures, "tracks-en.vtt"));
      await subs.getByRole("textbox", { name: "Label" }).fill("English");
      await subs.getByRole("button", { name: "Add subtitles" }).click();
      const rows = subs.locator("[data-ckui=subtitle]");
      await expect(rows).toHaveCount(1);
      await expect(rows.first()).toContainText("English");
      await subs.getByLabel("Subtitle file (.vtt, .srt)").setInputFiles(path.join(fixtures, "tracks-es.vtt"));
      await subs.getByRole("combobox", { name: "Language" }).click();
      await page.getByRole("option", { name: "Japanese" }).click();
      await subs.getByRole("switch", { name: /Forced/ }).click();
      await subs.getByRole("button", { name: "Add subtitles" }).click();
      await expect(rows).toHaveCount(2);
      await expect(rows.filter({ hasText: "Japanese" })).toContainText("Forced");
      await rows.filter({ hasText: "English" }).getByRole("button", { name: "Remove" }).click();
      await page.getByRole("alertdialog").getByRole("button", { name: "Remove" }).click();
      await expect(page.getByRole("alertdialog")).toBeHidden();
      await expect(rows).toHaveCount(1);
      await expect(state(page)).toHaveAttribute("data-state", "ready", { timeout: 60_000 });
      await page.screenshot({ path: `${dir}/media-item-panel-${mode}-${theme}-${info.project.name}.png`, fullPage: true });
    });
  }
}

test.describe("re-encode", () => {
  test.skip(({ isMobile }) => isMobile, "one Chromium run covers a re-encode");

  for (const mode of ["alone", "router"] as const) {
    test(`media item panel: replace the source and choose the poster (${mode})`, async ({ page }) => {
      const owner = await h.user();
      const video = await seedVideo(h, owner);
      await page.goto(admin(`/admin/media/video/${video.id}/${video.version}`, owner, { mode }));
      const panel = page.locator("[data-ckui=media-item-panel]");
      await expect(state(page)).toHaveAttribute("data-state", "ready", { timeout: 30_000 });
      const replacement = page.waitForResponse((r) => r.url().includes("/media/upload/commit") && r.request().postData()?.includes('"put"') === true);
      await panel.getByLabel("Video file").setInputFiles(path.join(fixtures, "portrait.mp4"));
      expect((await replacement).status()).toBe(200);
      // Processing may finish between polls; the source dimensions identify the replacement.
      await expect(panel.locator("[data-ckui=media-source]")).toContainText("360×640", { timeout: 180_000 });
      await expect(state(page)).toHaveAttribute("data-state", "ready", { timeout: 180_000 });

      await panel.getByRole("button", { name: "Choose poster" }).click();
      const picker = page.getByRole("dialog", { name: "Set cover" });
      await picker.getByRole("button", { name: "Use this frame" }).click({ timeout: 60_000 });
      await expect(picker).toBeHidden({ timeout: 60_000 });
      await expect(panel.locator("[data-ckui=media-poster] img")).toHaveAttribute("data-rendition", /^[0-9]+$/, { timeout: 90_000 });
    });
  }
});

for (const mode of ["alone", "router"] as const) {
  test(`media item panel: a failed encode shows why, and regenerate retries it (${mode})`, async ({ page }) => {
    const owner = await h.user();
    const it = await h.item({ kind: "video", owner: owner.id });
    const ref = mediaRef(it);
    // Not a video: the worker's probe fails it.
    await mediaClient(h, owner).put(new File([fixture("small.png")], "broken.mp4", { type: "video/mp4" }), { ref, path: "source", wait: false });
    await page.goto(admin(`/admin/media/video/${it.id}/${it.version}`, owner, { mode }));
    const panel = page.locator("[data-ckui=media-item-panel]");
    await expect(state(page)).toHaveAttribute("data-state", "failed", { timeout: 120_000 });
    await expect(panel.locator("[data-ckui=media-failure]")).toContainText("source");
    const commit = page.waitForResponse((r) => r.url().includes("/media/upload/commit") && r.request().postData()?.includes('"regenerate"') === true);
    await panel.getByRole("button", { name: "Regenerate" }).click();
    const res = await commit;
    expect(res.status()).toBe(200);
    // The commit answers the source retried: no failure, its presets pending again.
    const { files } = (await res.json()) as { files: { path: string; failed?: unknown; pending?: string[] }[] };
    const src = files.find((f) => f.path.startsWith("source"));
    expect(src?.failed).toBeUndefined();
    expect(src?.pending?.length).toBeGreaterThan(0);
    await expect(state(page)).toHaveAttribute("data-state", "failed", { timeout: 120_000 });
  });
}

test("someone who can't edit the item is told so", async ({ page }) => {
  const owner = await h.user();
  const it = await h.item({ kind: "video", owner: owner.id });
  await page.goto(admin(`/admin/media/video/${it.id}/${it.version}`, await h.user(), { mode: "alone" }));
  await expect(page.locator("[data-ckui=media-item-panel] [data-ckui=no-access]")).toContainText("You can't edit this item's media.", { timeout: 30_000 });
});

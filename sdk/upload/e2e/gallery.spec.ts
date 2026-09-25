import { expect, test, type Page } from "@playwright/test";

const dir = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";
const shot = (page: Page, name: string, full = false) => page.screenshot({ path: `${dir}/${name}.png`, fullPage: full });

for (const theme of ["light", "dark"] as const) {
  test(`media gallery: carousel, grid, lightbox (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await page.goto(`/gallery.html?theme=${theme}`);
    const post = page.locator("[data-demo=post]");
    const gallery = post.locator("[data-ckui=media-gallery]");
    await expect(gallery).toHaveAttribute("data-view", "carousel");
    await expect(post.getByText("1 / 6")).toBeVisible();
    await expect(post.locator("[data-ckui=slide][data-current] img")).toHaveJSProperty("complete", true);
    // The stage spans the column at the first item's aspect (4:3).
    const stage = await post.locator("[data-ckui=slide][data-current]").boundingBox();
    const column = await gallery.boundingBox();
    expect(Math.abs(stage!.width - column!.width)).toBeLessThan(1);
    expect(stage!.width / stage!.height).toBeCloseTo(4 / 3, 1);
    await shot(page, `gallery-carousel-${tag}`, true);

    // Slide 2: the landscape video plays in place.
    await post.getByRole("button", { name: "Next" }).click();
    await expect(post.getByText("2 / 6")).toBeVisible();
    const player = post.locator("[data-ckui=slide][data-current] [data-ckui=video-player]");
    await expect(player.locator("[data-ckui=sprite-frame]")).toBeVisible();
    await player.getByRole("button", { name: "Play video" }).click();
    await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
    await page.waitForTimeout(600);
    await post.screenshot({ path: `${dir}/gallery-video-playing-${tag}.png` });

    // Swiping/keying away pauses it; the portrait video letterboxes in the same stage.
    await post.locator("[data-ckui=carousel]").focus();
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("ArrowRight");
    await expect(post.getByText("4 / 6")).toBeVisible();
    expect(await player.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true);
    await page.waitForTimeout(400);
    await post.screenshot({ path: `${dir}/gallery-portrait-${tag}.png` });

    // Touch swipe back (mobile) or drag (desktop).
    const box = (await post.locator("[data-ckui=slide][data-current]").boundingBox())!;
    await page.mouse.move(box.x + box.width * 0.8, box.y + 40);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width * 0.5, box.y + 42, { steps: 4 });
    await page.mouse.move(box.x + box.width * 0.2, box.y + 44, { steps: 4 });
    await page.mouse.up();
    await expect(post.getByText("5 / 6")).toBeVisible();

    // Grid, remembered across reloads.
    await post.getByRole("button", { name: "Grid" }).click();
    await expect(gallery).toHaveAttribute("data-view", "grid");
    await page.reload();
    await expect(gallery).toHaveAttribute("data-view", "grid");
    const tiles = post.locator("[data-ckui=tile]");
    await expect(tiles).toHaveCount(6);
    await expect(tiles.nth(1).locator("[data-ckui=sprite-frame]")).toBeVisible();
    await expect(tiles.nth(1)).toContainText("0:06");
    await page.waitForTimeout(300);
    await post.screenshot({ path: `${dir}/gallery-grid-${tag}.png` });

    // Lightbox at the clicked tile; Tab stays inside; Esc returns focus.
    await tiles.nth(2).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("3 / 6")).toBeVisible();
    await expect(dialog.locator("[data-ckui=slide][data-current] img")).toHaveJSProperty("complete", true);
    await page.waitForTimeout(300);
    await shot(page, `gallery-lightbox-${tag}`);
    await page.keyboard.press("ArrowRight");
    await expect(dialog.getByText("4 / 6")).toBeVisible();
    await page.waitForTimeout(300);
    await shot(page, `gallery-lightbox-video-${tag}`);
    for (let i = 0; i < 8; i++) {
      await page.keyboard.press("Tab");
      // Base UI's focus guards (outside the popup) hand focus straight back in.
      const where = await dialog.evaluate((d) => (d.contains(document.activeElement) || document.activeElement?.hasAttribute("data-base-ui-focus-guard") ? "in" : document.activeElement?.outerHTML.slice(0, 120)));
      expect(where).toBe("in");
    }
    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(tiles.nth(2)).toBeFocused();
    await post.getByRole("button", { name: "Carousel" }).click();

    // Locked post and a lone vertical clip.
    const locked = page.locator("[data-demo=locked]");
    await expect(locked.getByRole("button", { name: "Unlock 5 for $5" })).toBeVisible();
    await locked.screenshot({ path: `${dir}/gallery-locked-${tag}.png` });
    const single = page.locator("[data-demo=single]");
    await expect(single.getByRole("button", { name: "Grid" })).toHaveCount(0);
    const clip = (await single.locator("[data-ckui=video-player]").boundingBox())!;
    expect(clip.height).toBeLessThanOrEqual(page.viewportSize()!.height * 0.8 + 1);
    await single.screenshot({ path: `${dir}/gallery-single-${tag}.png` });
  });
}

test("player fails fast with a specific reason", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop");
  const warnings: string[] = [];
  page.on("console", (m) => m.type() === "warning" && warnings.push(m.text()));
  await page.goto("/gallery.html?page=player");
  const player = (demo: string) => page.locator(`[data-demo=${demo}] [data-ckui=video-player]`);

  const started = Date.now();
  await player("no-cors").getByRole("button", { name: "Play video" }).click();
  const alert = player("no-cors").getByRole("alert");
  await expect(alert).toContainText("The video server couldn't be reached.", { timeout: 10_000 });
  expect(Date.now() - started).toBeLessThan(10_000);
  await expect(alert.getByRole("button", { name: "Try again" })).toBeVisible();
  await expect(alert).toContainText(/Code fragLoadError\/0/);
  expect(warnings.some((w) => w.includes("MEDIA_ACCESS_CORS_ORIGINS"))).toBe(true);
  await player("no-cors").screenshot({ path: `${dir}/player-error-network.png` });
  await alert.getByRole("button", { name: "Try again" }).click();
  await expect(player("no-cors").getByRole("alert")).toBeVisible({ timeout: 10_000 });

  // 404 (expired token) → the host refreshes the grant → plays.
  await player("refresh").getByRole("button", { name: "Play video" }).click();
  await expect(player("refresh")).toHaveAttribute("data-status", "playing", { timeout: 10_000 });

  await player("denied").getByRole("button", { name: "Play video" }).click();
  await expect(player("denied").getByRole("alert")).toContainText("This video isn't available.", { timeout: 10_000 });

  await player("busy").getByRole("button", { name: "Play video" }).click();
  await expect(player("busy").getByRole("alert")).toContainText("Too many requests. Try again shortly.", { timeout: 10_000 });

  await player("missing").getByRole("button", { name: "Play video" }).click();
  await expect(player("missing").getByRole("alert")).toContainText("This video isn't available.", { timeout: 10_000 });
  await player("missing").screenshot({ path: `${dir}/player-error-missing.png` });
});

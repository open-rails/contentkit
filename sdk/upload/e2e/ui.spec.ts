import { expect, test, type Page } from "@playwright/test";
import path from "node:path";

// Saves encode in the page; slow shared runners need the headroom.
test.describe.configure({ timeout: 90_000 });

const dir = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";
const fixture = (n: string) => path.join(import.meta.dirname, "fixtures", n);

async function shot(page: Page, name: string, project: string) {
  await page.screenshot({ path: `${dir}/${name}-${project}.png`, fullPage: true });
}

for (const theme of ["light", "dark"] as const) {
  test(`profile page and crop dialogs (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await page.goto(`/?theme=${theme}`);
    const cover = page.locator("[data-ckui=cover-upload]").first();
    const avatar = page.locator("[data-ckui=avatar-upload]").first();
    await expect(cover.locator("img")).toHaveAttribute("srcset", /1500w/);
    await expect(avatar.locator("img")).toHaveAttribute("srcset", /512w/);
    await expect(cover.locator("img")).toHaveJSProperty("complete", true);
    await shot(page, "profile", tag);
    const encode = page.locator("[data-demo=encode]");
    await expect(encode.getByText("Encoding · segment 5 / 27 · ~40 s left")).toBeVisible();
    await expect(encode.getByText("Queued · #3 in line")).toBeVisible();
    await encode.screenshot({ path: `${dir}/encode-progress-${tag}.png` });

    await cover.getByRole("button", { name: "Edit crop" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Crop your cover")).toBeVisible();
    await expect(dialog.locator(".reactEasyCrop_CropArea")).toBeVisible();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${dir}/crop-cover-${tag}.png` });
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog).toBeHidden();

    // A phone portrait stored sideways (EXIF orientation 6): the preview must be upright.
    const empty = page.locator("[data-ckui=avatar-upload]").nth(1);
    await empty.locator("input[type=file]").setInputFiles(fixture("portrait-exif6.jpg"));
    await expect(dialog.getByText("Crop your avatar")).toBeVisible();
    const media = dialog.locator(".reactEasyCrop_Image");
    await expect(media).toBeVisible();
    const box = await media.boundingBox();
    expect(box!.height).toBeGreaterThan(box!.width);
    await dialog.getByRole("button", { name: "Zoom in" }).click();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${dir}/crop-avatar-${tag}.png` });
    // A quarter turn lays the portrait on its side.
    await dialog.getByRole("button", { name: "Rotate right" }).click();
    await page.waitForTimeout(300);
    const turned = (await media.evaluate((el) => el.getBoundingClientRect()))!;
    expect(turned.width).toBeGreaterThan(turned.height);
    await page.screenshot({ path: `${dir}/crop-rotated-${tag}.png` });
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    await expect(empty.locator("img")).toHaveAttribute("srcset", /512w/);

    // Too small for the largest output: the dialog warns.
    const emptyCover = page.locator("[data-ckui=cover-upload]").nth(1);
    await emptyCover.locator("input[type=file]").setInputFiles(fixture("small.png"));
    await expect(dialog.locator("[data-ckui=undersized]")).toBeVisible();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${dir}/crop-undersized-${tag}.png` });
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    await shot(page, "after-upload", tag);
  });
}

for (const theme of ["light", "dark"] as const) {
  test(`SlotEditor overlay header (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await page.goto(`/?theme=${theme}`);
    const header = page.locator("[data-demo=header]");
    await expect(header.locator("img").first()).toHaveAttribute("srcset", /1500w/, { timeout: 20_000 });
    await expect(header.locator("img").nth(1)).toHaveAttribute("srcset", /512w/);
    // Host-styled triggers: no kit wrapper, the menu popup is scoped.
    const coverTrigger = header.getByRole("button", { name: "Change cover" });
    await expect(coverTrigger).not.toHaveClass(/ckui/);
    await header.screenshot({ path: `${dir}/header-${tag}.png` });

    await coverTrigger.click();
    const menu = page.getByRole("menu");
    await expect(menu.getByRole("menuitem", { name: "Edit crop" })).toBeVisible();
    await page.waitForTimeout(200);
    await page.screenshot({ path: `${dir}/header-menu-${tag}.png` });
    await menu.getByRole("menuitem", { name: "Edit crop" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Crop your cover")).toBeVisible();
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });

    await header.getByRole("button", { name: "Change avatar" }).click();
    await menu.getByRole("menuitem", { name: "Change" }).click();
    await header.locator("input[type=file]").nth(1).setInputFiles(fixture("portrait-exif6.jpg"));
    await expect(dialog.getByText("Crop your avatar")).toBeVisible();
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    await expect(header.locator("img").nth(1)).toHaveAttribute("srcset", /512w/);
  });
}

for (const theme of ["light", "dark"] as const) {
  test(`video poster and hover preview pickers (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await page.goto(`/?theme=${theme}`);
    const card = page.locator("[data-demo=video]");
    const poster = card.locator("[data-ckui=video-poster]");
    await expect(poster.locator("img")).toHaveAttribute("srcset", /1920w/, { timeout: 20_000 });
    await poster.scrollIntoViewIfNeeded();
    await poster.hover();
    await expect(poster.locator("[data-ckui=hover-preview]")).toBeVisible();
    await card.screenshot({ path: `${dir}/video-card-hover-${tag}.png` });
    await page.mouse.move(0, 0);
    await expect(poster.locator("[data-ckui=hover-preview]")).toHaveCount(0);

    await card.getByRole("button", { name: "Set cover" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.locator("[data-ckui=frame-strip] img")).toHaveCount(8, { timeout: 20_000 });
    await dialog.getByRole("button", { name: /Jump to 0:08/ }).click();
    await dialog.getByRole("button", { name: "Next frame" }).click();
    await expect(dialog.locator("[data-ckui=time]")).toHaveText("0:08.3");
    await page.waitForTimeout(600);
    await page.screenshot({ path: `${dir}/poster-picker-${tag}.png` });
    await dialog.getByRole("button", { name: "Crop…" }).click();
    const crop = page.getByRole("dialog", { name: "Crop the cover" });
    await expect(crop.locator(".reactEasyCrop_CropArea")).toBeVisible();
    await crop.getByRole("button", { name: "Zoom in" }).click();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${dir}/poster-crop-${tag}.png` });
    await crop.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0, { timeout: 30_000 });
    await expect(poster.locator("img")).toHaveAttribute("srcset", /1920w/);

    await card.getByRole("button", { name: "Hover preview" }).click();
    await expect(dialog.locator("[data-ckui=frame-strip] img")).toHaveCount(10, { timeout: 20_000 });
    const [, end] = await dialog.locator("input[type=range]").all();
    await end!.focus();
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("ArrowRight");
    await expect(dialog.locator("[data-ckui=flipbook]")).toBeVisible({ timeout: 20_000 });
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${dir}/preview-picker-${tag}.png` });
    await dialog.getByRole("button", { name: "Save preview" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0, { timeout: 30_000 });
  });
}

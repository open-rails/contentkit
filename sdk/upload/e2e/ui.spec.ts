import { expect, test, type Page } from "@playwright/test";
import path from "node:path";

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

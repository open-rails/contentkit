import { expect, test, type Locator, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import path from "node:path";

const dir = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";
const img = (n: string) => ({ name: n, mimeType: "image/jpeg", buffer: readFileSync(path.join(import.meta.dirname, "fixtures", "media", "img", n)) });
const names = (editor: Locator) => () => editor.locator("[data-ckui=upload-name]").allTextContents();
const row = (editor: Locator, name: string) => editor.locator("[data-ckui=upload-row]").filter({ has: editor.page().getByText(name, { exact: true }) });

async function moveWithKeyboard(page: Page, handle: Locator, steps: number) {
  await handle.focus();
  await page.keyboard.press("Space");
  for (let i = 0; i < steps; i++) {
    await page.waitForTimeout(80);
    await page.keyboard.press("ArrowDown");
  }
  await page.waitForTimeout(80);
  await page.keyboard.press("Space");
}

for (const theme of ["light", "dark"] as const) {
  test(`media folder: upload, reorder, crop, rename, remove, and the post follows (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await page.goto(`/folder.html?theme=${theme}`);
    const editor = page.locator("[data-demo=editor] [data-ckui=media-folder]");
    const post = page.locator("[data-demo=post]");
    await expect(editor.getByText("Drop files here, or click to choose")).toBeVisible();
    await expect(editor.getByText("Images up to 25 MB, at most 6 · Videos up to 1 GB, at most 2 · video shapes from 1:2.4 to 2.4:1")).toBeVisible();
    await page.screenshot({ path: `${dir}/folder-empty-${tag}.png`, fullPage: true });

    // A type no path takes is refused before it uploads.
    await editor.locator("[data-ckui=folder-drop] input[type=file]").setInputFiles([
      img("1.jpg"),
      img("2.jpg"),
      img("3.jpg"),
      { name: "loop.gif", mimeType: "image/gif", buffer: Buffer.from("GIF89a") },
    ]);
    await expect(editor.locator("[data-ckui=folder-refused]")).toContainText("loop.gif: This file type isn't supported here.");
    const add = editor.getByRole("button", { name: "Add 3" });
    await expect(add).toBeEnabled({ timeout: 15_000 });
    await editor.screenshot({ path: `${dir}/folder-queue-${tag}.png` });
    await add.click();

    const rows = editor.locator("[data-ckui=upload-row]");
    await expect(rows).toHaveCount(3);
    await expect.poll(names(editor)).toEqual(["1.jpg", "2.jpg", "3.jpg"]);
    // Processed: thumbnails, and the post shows the three images.
    await expect(rows.first().locator("[data-ckui=thumb] img")).toBeVisible({ timeout: 15_000 });
    await expect(post.getByText("1 / 3")).toBeVisible({ timeout: 15_000 });
    await expect(editor.locator("[data-ckui=folder-refused]")).toBeVisible();
    await editor.getByRole("button", { name: "Dismiss" }).click();

    // Keyboard: lift 1.jpg, move it down twice, drop.
    await moveWithKeyboard(page, editor.getByRole("button", { name: "Reorder 1.jpg" }), 2);
    await expect.poll(names(editor)).toEqual(["2.jpg", "3.jpg", "1.jpg"]);
    if (!info.project.use.isMobile) {
      // Pointer: drag 1.jpg back above 2.jpg.
      await editor.getByRole("button", { name: "Reorder 1.jpg" }).dragTo(editor.getByRole("button", { name: "Reorder 2.jpg" }), { steps: 12 });
      await expect.poll(names(editor)).toEqual(["1.jpg", "2.jpg", "3.jpg"]);
    }

    await row(editor, "2.jpg").getByRole("button", { name: "Crop and rotate" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("button", { name: "Original" })).toHaveAttribute("aria-pressed", "true");
    await dialog.getByRole("button", { name: "1:1" }).click();
    await expect(dialog.locator("[data-ckui=crop-preview] [role=img]")).toBeVisible();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${dir}/folder-crop-${tag}.png` });
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog).toBeHidden();
    await expect(row(editor, "2.jpg").getByText("Edited")).toBeVisible();

    await row(editor, "3.jpg").getByRole("button", { name: "Rename" }).click();
    const input = editor.getByRole("textbox", { name: "New name for 3.jpg" });
    await input.fill("cover");
    await input.press("Enter");
    await expect.poll(names(editor)).toContain("cover.jpg");
    await editor.screenshot({ path: `${dir}/folder-editor-${tag}.png` });

    page.once("dialog", (d) => void d.accept());
    await row(editor, "cover.jpg").getByRole("checkbox").check();
    await row(editor, "1.jpg").getByRole("checkbox").check();
    await expect(editor.getByText("2 selected")).toBeVisible();
    await editor.getByRole("button", { name: "Remove selected" }).click();
    await expect(rows).toHaveCount(1);
    await expect.poll(names(editor)).toEqual(["2.jpg"]);
    // One item left: the post shows it alone, without a counter.
    await expect(post.getByText(/\/ 3/)).toHaveCount(0);
    await expect(post.locator("[data-ckui=slide] img")).toHaveCount(1);
  });
}

test("a draft adds files as they finish; discarding stops an upload in flight", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "one browser covers the draft flow");
  await page.goto("/folder.html?delay=600");
  const composer = page.locator("[data-demo=composer]");
  await composer.locator("[data-ckui=folder-drop] input[type=file]").setInputFiles([img("1.jpg"), img("2.jpg")]);
  await expect(composer.locator("[data-ckui=upload-row]")).toHaveCount(2, { timeout: 20_000 });
  await expect(composer.getByRole("button", { name: /^Add \d/ })).toHaveCount(0);

  await composer.locator("[data-ckui=folder-add] input").setInputFiles([img("4.jpg")]);
  await expect(composer.locator("[data-ckui=queue-row]")).toHaveCount(1);
  await composer.getByRole("button", { name: "Discard draft" }).click();
  await expect(composer.getByText("Drop files here, or click to choose")).toBeVisible();
  await page.waitForTimeout(2000);
  await expect(composer.locator("[data-ckui=upload-row]")).toHaveCount(0);
  await expect(composer.locator("[data-ckui=queue-row]")).toHaveCount(0);
});

test("the folder editor speaks the page's language", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop");
  await page.goto("/folder.html?lang=ja");
  const editor = page.locator("[data-demo=editor] [data-ckui=media-folder]");
  await expect(editor.getByText("ここにファイルをドロップするか、クリックして選択")).toBeVisible();
  await editor.locator("input[type=file]").first().setInputFiles([img("1.jpg")]);
  await expect(editor.getByRole("button", { name: "1 件を追加" })).toBeEnabled({ timeout: 15_000 });
});

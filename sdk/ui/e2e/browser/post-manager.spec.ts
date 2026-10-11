import path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { contentKit } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// PostManager with the package's rich-text editor in demo/admin.tsx: alone
// and in React Router.
test.describe.configure({ timeout: 180_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const small = path.resolve(import.meta.dirname, "..", "fixtures", "small.png");

/** "YYYY-MM-DDTHH:mm" in local time, as a datetime-local input takes it. */
const localInput = (d: Date) => new Date(d.getTime() - d.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);

const imagesLoaded = (p: Page) =>
  p.locator("[data-ckui=rich-text-editor] .ProseMirror img").evaluateAll((imgs) => imgs.length > 0 && imgs.every((i) => (i as HTMLImageElement).complete && (i as HTMLImageElement).naturalWidth > 0));

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`post manager: draft, inline image, schedule, publish, delete and restore (${mode}, ${theme})`, async ({ page }, info) => {
      const editor = await h.user({ role: "editor" });
      const tag = Math.random().toString(36).slice(2, 8);
      await page.goto(admin("/admin/posts", editor, { mode, theme, params: { q: tag } }));
      const flow = page.locator("[data-ckui=post-manager]");
      await expect(flow.getByText("No posts match.")).toBeVisible({ timeout: 30_000 });
      expect(await inheritsHostTheme(page, "post-manager")).toBe(true);

      // A draft: title and body first, then images once it exists.
      await flow.getByRole("button", { name: "New post" }).click();
      await expect(page).toHaveURL(/\/admin\/posts\/new$/);
      await flow.getByRole("textbox", { name: "Title" }).fill(`Launch notes ${tag}`);
      const body = flow.getByRole("textbox", { name: "Body" });
      await body.click();
      await page.keyboard.type("First paragraph.");
      await flow.getByRole("button", { name: "Bold" }).click();
      await page.keyboard.type(" Bold words.");
      await flow.getByRole("button", { name: "Create draft" }).click();
      await expect(page).toHaveURL(/\/admin\/posts\/[0-9a-f-]{36}$/);
      const id = page.url().split("/").pop()!;
      const form = flow.locator("[data-ckui=post-form]");
      await expect(form.getByText("Draft", { exact: true })).toBeVisible();
      await expect(body.locator("strong")).toHaveText("Bold words.");

      await body.click();
      await page.keyboard.press("End");
      await flow.locator("[data-ckui=rich-text-editor] input[type=file]").setInputFiles(small);
      await expect.poll(() => imagesLoaded(page), { timeout: 30_000 }).toBe(true);
      await flow.getByRole("button", { name: "Save", exact: true }).click();
      await expect(flow.getByText("Saved.")).toBeVisible();
      const ck = contentKit(h, editor);
      await expect.poll(async () => (await ck.posts.get(id)).body, { timeout: 15_000 }).toMatch(/<img src="contentkit:i-[0-9a-f-]{36}"/);

      // A deep link opens the draft with its image (an editor view).
      await page.goto(admin(`/admin/posts/${id}`, editor, { mode, theme }));
      await expect(flow.getByRole("textbox", { name: "Title" })).toHaveValue(`Launch notes ${tag}`, { timeout: 30_000 });
      await expect.poll(() => imagesLoaded(page), { timeout: 60_000 }).toBe(true);
      await page.screenshot({ path: `${dir}/post-manager-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

      // Scheduled, then published now.
      await flow.getByLabel("Goes live (optional)").fill(localInput(new Date(Date.now() + 3 * 86_400_000)));
      await flow.getByRole("button", { name: "Publish" }).click();
      await expect(form.getByText("Scheduled", { exact: true })).toBeVisible();
      await flow.getByLabel("Goes live (optional)").fill(localInput(new Date(Date.now() - 60_000)));
      await flow.getByRole("button", { name: "Save", exact: true }).click();
      await expect(form.getByText("Published", { exact: true })).toBeVisible();

      // The list, then delete and restore.
      await flow.getByRole("button", { name: "All posts" }).click();
      await page.goto(admin("/admin/posts", editor, { mode, theme, params: { q: tag } }));
      const row = flow.locator("[data-ckui=post-row]", { hasText: `Launch notes ${tag}` });
      await expect(row).toHaveAttribute("data-status", "published", { timeout: 30_000 });
      await row.getByRole("button", { name: "Delete" }).click();
      await page.getByRole("alertdialog").getByRole("button", { name: "Delete post" }).click();
      await expect(row).toHaveCount(0);
      await flow.getByRole("button", { name: "Deleted" }).click();
      await expect(page).toHaveURL(/deleted=1/);
      await row.getByRole("button", { name: "Restore" }).click();
      await expect(row).toHaveCount(0);
      await flow.getByRole("button", { name: "Posts", exact: true }).click();
      await expect(row).toHaveAttribute("data-status", "published");
    });
  }
}

test("a caller without post permission is told so", async ({ page }) => {
  await page.goto(admin("/admin/posts", await h.user({ role: "moderator" }), { mode: "alone" }));
  await expect(page.locator("[data-ckui=post-manager] [data-ckui=no-access]")).toContainText("You don't have permission to manage posts.", { timeout: 30_000 });
});

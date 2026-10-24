import { expect, test } from "@playwright/test";
import { seedModeration } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// CommentModeration in demo/admin.tsx: alone over the history API, and in
// React Router through `navigation`. Sections, filters and pages are URL state.
test.describe.configure({ timeout: 90_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`comment moderation keeps its section, filters and page in the URL (${mode}, ${theme})`, async ({ page }, info) => {
      const { staff, bob, tag } = await seedModeration(h);
      await page.goto(admin("/admin/comments", staff, { mode, theme, params: { size: "2" } }));
      const mod = page.locator("[data-ckui=comment-moderation]");
      const rows = mod.locator("[data-ckui=admin-comment]");
      const where = page.locator("[data-demo=router-location]");
      await expect(mod.getByRole("tab", { name: "All comments" })).toHaveAttribute("aria-selected", "true", { timeout: 30_000 });
      expect(await inheritsHostTheme(page, "comment-moderation")).toBe(true);

      // Filter by author: the list, its page and the URL follow.
      await mod.getByRole("textbox", { name: "Author" }).fill(bob.id);
      await mod.getByRole("button", { name: "Filter" }).click();
      await expect(page).toHaveURL(new RegExp(`/admin/comments\\?author=${bob.id}$`));
      await expect(mod.getByText(`Comments by ${bob.username}`)).toBeVisible();
      await expect(rows).toHaveText([new RegExp(`Bob three ${tag}`), new RegExp(`Bob two ${tag}`)]);
      await mod.getByRole("button", { name: "Next" }).click();
      await expect(page).toHaveURL(new RegExp(`author=${bob.id}&page=2$`));
      await expect(mod.getByText("Page 2")).toBeVisible();
      await expect(rows).toHaveText([new RegExp(`Bob one ${tag}`)]);
      if (mode === "router") await expect(where).toHaveText(`/admin/comments?author=${bob.id}&page=2`);
      await page.screenshot({ path: `${dir}/moderation-url-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

      // Back and forward walk the history.
      await page.goBack();
      await expect(rows).toHaveCount(2);
      await expect(mod.getByText("Page 1")).toBeVisible();
      await page.goForward();
      await expect(rows).toHaveText([new RegExp(`Bob one ${tag}`)]);

      // Sections are paths under basePath.
      await mod.getByRole("tab", { name: "Bans", exact: true }).click();
      await expect(page).toHaveURL(/\/admin\/comments\/bans$/);
      await expect(mod.locator("[data-ckui=comment-bans]")).toBeVisible();
      await page.goBack();
      await expect(mod.getByRole("tab", { name: "All comments" })).toHaveAttribute("aria-selected", "true");
      await expect(rows).toHaveText([new RegExp(`Bob one ${tag}`)]);

      // A deep link opens the section it names.
      await page.goto(admin("/admin/comments/queue", staff, { mode, theme }));
      await expect(mod.getByRole("tab", { name: "Review queue" })).toHaveAttribute("aria-selected", "true", { timeout: 30_000 });
      await expect(page).toHaveURL(/\/admin\/comments\/queue/);

      if (mode === "router") {
        // The host's own links move the flow: one source of truth.
        await page.locator("[data-demo=host-nav]").getByRole("link", { name: "Bans" }).click();
        await expect(mod.getByRole("tab", { name: "Bans", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(where).toHaveText("/admin/comments/bans");
      }
    });
  }
}

test("a row's author filters the list to their comments", async ({ page }) => {
  const { staff, bob, tag } = await seedModeration(h);
  await page.goto(admin("/admin/comments", staff, { mode: "alone" }));
  const mod = page.locator("[data-ckui=comment-moderation]");
  const row = mod.locator("[data-ckui=admin-comment]", { hasText: `Bob two ${tag}` });
  await row.getByRole("button", { name: "Their comments" }).click({ timeout: 30_000 });
  await expect(page).toHaveURL(new RegExp(`author=${bob.id}`));
  await expect(mod.locator("[data-ckui=admin-comment]")).toHaveCount(3);
  await mod.getByRole("button", { name: "Show everyone's" }).click();
  await expect(page).toHaveURL(/\/admin\/comments$/);
});

test("a caller without comment permissions is told so", async ({ page }) => {
  await page.goto(admin("/admin/comments", await h.user({ role: "editor" }), { mode: "alone" }));
  const mod = page.locator("[data-ckui=comment-moderation]");
  await expect(mod.locator("[data-ckui=no-access]")).toContainText("You don't have permission to moderate comments.", { timeout: 30_000 });
  await expect(mod.getByRole("tab")).toHaveCount(0);
});

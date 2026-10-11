import { expect, test } from "@playwright/test";
import { seedModeration } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// UserCommentStanding in demo/admin.tsx's user page: alone (baseUrl and auth)
// and in React Router (client).
test.describe.configure({ timeout: 90_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`a user's comment standing: ban, replace, lift; delete and restore their comments (${mode}, ${theme})`, async ({ page }, info) => {
      const { staff, bob, tag } = await seedModeration(h);
      await page.goto(admin(`/admin/users/${bob.id}`, staff, { mode, theme, params: { name: bob.username } }));
      const panel = page.locator("[data-ckui=user-comment-standing]");
      const ban = panel.locator("[data-ckui=standing-ban]");
      await expect(ban.getByText("Not banned: they can comment.")).toBeVisible({ timeout: 30_000 });
      expect(await inheritsHostTheme(page, "user-comment-standing")).toBe(true);

      await ban.getByRole("button", { name: "Ban from commenting" }).click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByRole("heading")).toHaveText(`Ban ${bob.username} from commenting`);
      await dialog.getByRole("button", { name: "30 days" }).click();
      await dialog.getByRole("textbox", { name: "Reason (optional)" }).fill("Spam links");
      await dialog.getByRole("button", { name: "Ban", exact: true }).click();
      await expect(ban.getByText("Active")).toBeVisible();
      await expect(ban.getByText("Reason: Spam links")).toBeVisible();

      await ban.getByRole("button", { name: "Update ban" }).click();
      await dialog.getByRole("button", { name: "Until lifted" }).click();
      await dialog.getByRole("button", { name: "Update ban" }).click();
      await expect(ban.getByText(/Until it is lifted\./)).toBeVisible();
      await page.screenshot({ path: `${dir}/user-comment-standing-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

      await ban.getByRole("button", { name: "Lift ban" }).click();
      await page.getByRole("alertdialog").getByRole("button", { name: "Lift ban" }).click();
      await expect(ban.getByText("Not banned: they can comment.")).toBeVisible();

      const comments = panel.locator("[data-ckui=standing-comments] [data-ckui=admin-comment]");
      await expect(comments).toHaveText([new RegExp(`Bob three ${tag}`), new RegExp(`Bob two ${tag}`), new RegExp(`Bob one ${tag}`)]);
      const two = comments.filter({ hasText: `Bob two ${tag}` });
      await two.getByRole("button", { name: "Delete" }).click();
      await expect(two).toHaveAttribute("data-status", "deleted");
      await two.getByRole("button", { name: "Restore" }).click();
      await expect(two).toHaveAttribute("data-status", "published");
    });
  }
}

test("a user page without comment permissions says so", async ({ page }) => {
  const { bob } = await seedModeration(h);
  await page.goto(admin(`/admin/users/${bob.id}`, await h.user({ role: "editor" }), { mode: "router" }));
  await expect(page.locator("[data-ckui=user-comment-standing] [data-ckui=no-access]")).toContainText("You don't have permission to manage comments.", { timeout: 30_000 });
});

test("a moderator's user page in Japanese", async ({ page }) => {
  const { staff, bob } = await seedModeration(h);
  await page.goto(admin(`/admin/users/${bob.id}`, staff, { mode: "alone", params: { lang: "ja" } }));
  const panel = page.locator("[data-ckui=user-comment-standing]");
  await expect(panel.getByRole("heading", { name: "コメント", exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(panel.getByText("禁止されていません。コメントできます。")).toBeVisible();
});

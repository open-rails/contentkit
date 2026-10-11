import { expect, test } from "@playwright/test";
import { seedPolls } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// PollManager in demo/admin.tsx: alone over the history API, and in React
// Router through `navigation`. Views and filters are URL state.
test.describe.configure({ timeout: 120_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`poll manager: filter, create, edit and delete, in the URL (${mode}, ${theme})`, async ({ page }, info) => {
      const { staff, tag } = await seedPolls(h);
      await page.goto(admin("/admin/polls", staff, { mode, theme, params: { q: tag } }));
      const flow = page.locator("[data-ckui=poll-manager]");
      const rows = flow.locator("[data-ckui=poll-row]");
      await expect(rows).toHaveCount(3, { timeout: 30_000 });
      expect(await inheritsHostTheme(page, "poll-manager")).toBe(true);
      await expect(rows.filter({ hasText: `Scheduled ${tag}?` })).toHaveAttribute("data-state", "scheduled");
      await expect(rows.filter({ hasText: `Inactive ${tag}?` })).toHaveAttribute("data-state", "inactive");

      await flow.getByRole("combobox", { name: "State" }).click();
      await page.getByRole("option", { name: "Scheduled" }).click();
      await expect(page).toHaveURL(new RegExp(`/admin/polls\\?q=${tag}&state=scheduled$`));
      await expect(rows).toHaveText([new RegExp(`Scheduled ${tag}`)]);
      await page.goBack();
      await expect(rows).toHaveCount(3);
      await flow.getByRole("combobox", { name: "Language" }).click();
      await page.getByRole("option", { name: "日本語" }).click();
      await expect(rows).toHaveText([new RegExp(`Inactive ${tag}`)]);
      await page.screenshot({ path: `${dir}/poll-manager-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

      await flow.getByRole("button", { name: "New poll" }).click();
      await expect(page).toHaveURL(/\/admin\/polls\/new$/);
      await flow.getByRole("textbox", { name: "Question" }).fill(`Created ${tag}?`);
      await flow.getByRole("textbox", { name: "Option 1" }).fill("Red");
      await flow.getByRole("textbox", { name: "Option 2" }).fill("Blue");
      await flow.getByRole("button", { name: "Create poll" }).click();
      await expect(page).toHaveURL(/\/admin\/polls\/[0-9a-f-]{36}$/);
      await expect(flow.getByRole("heading", { name: "Edit poll" })).toBeVisible();
      if (mode === "router") await expect(page.locator("[data-demo=router-location]")).toHaveText(/^\/admin\/polls\/[0-9a-f-]{36}$/);
      await flow.getByRole("textbox", { name: "Question" }).fill(`Edited ${tag}?`);
      await flow.getByRole("button", { name: "Save", exact: true }).click();
      await expect(flow.getByText("Saved.")).toBeVisible();

      await flow.getByRole("button", { name: "All polls" }).click();
      await expect(page).toHaveURL(/\/admin\/polls$/);
      await page.goto(admin("/admin/polls", staff, { mode, theme, params: { q: tag } }));
      const edited = rows.filter({ hasText: `Edited ${tag}?` });
      await expect(edited).toHaveAttribute("data-state", "live", { timeout: 30_000 });
      await edited.getByRole("button", { name: "Delete" }).click();
      const confirm = page.getByRole("alertdialog");
      await expect(confirm.getByRole("button", { name: "Delete poll" })).toBeDisabled();
      await confirm.getByRole("textbox", { name: "Type delete to confirm" }).fill("delete");
      await confirm.getByRole("button", { name: "Delete poll" }).click();
      await expect(edited).toHaveCount(0);
      await expect(rows).toHaveCount(3);
    });
  }
}

test("a caller without poll permission is told so", async ({ page }) => {
  await page.goto(admin("/admin/polls", await h.user({ role: "moderator" }), { mode: "router" }));
  await expect(page.locator("[data-ckui=poll-manager] [data-ckui=no-access]")).toContainText("You don't have permission to manage polls.", { timeout: 30_000 });
});

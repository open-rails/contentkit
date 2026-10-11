import { expect, test } from "@playwright/test";
import path from "node:path";
import { seedTaxonomy } from "../support/seed.ts";
import { admin, harness, inheritsHostTheme, screenshots as dir } from "./pages.ts";

// TaxonomyManager in demo/admin.tsx: tags, series (with covers) and
// characters (members of series), alone and in React Router.
test.describe.configure({ timeout: 150_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();

for (const mode of ["alone", "router"] as const) {
  for (const theme of ["light", "dark"] as const) {
    test(`taxonomy manager: create, names, alias, relation, merge, contents (${mode}, ${theme})`, async ({ page }, info) => {
      const { staff, tag, color, colour } = await seedTaxonomy(h);
      const flow = page.locator("[data-ckui=taxonomy-manager]");
      const rows = flow.locator("[data-ckui=node-row]");
      await page.goto(admin("/admin/taxonomy/character", staff, { mode, theme, params: { q: tag } }));
      await expect(rows).toHaveText([new RegExp(`Aria ${tag}`), new RegExp(`Bram ${tag}`)], { timeout: 30_000 });
      expect(await inheritsHostTheme(page, "taxonomy-manager")).toBe(true);
      await expect(flow.getByRole("tab", { name: "Characters" })).toHaveAttribute("aria-selected", "true");

      // Create with a name per language; the slug follows the English one.
      await flow.getByRole("button", { name: "New", exact: true }).click();
      await expect(page).toHaveURL(/\/admin\/taxonomy\/character\/new$/);
      await flow.getByRole("textbox", { name: "Name in English" }).fill(`Cleo ${tag}`);
      await flow.getByRole("textbox", { name: "Name in 日本語" }).fill(`クレオ ${tag}`);
      await expect(flow.getByRole("textbox", { name: "Slug" })).toHaveValue(`cleo-${tag}`);
      await flow.getByRole("button", { name: "Create", exact: true }).click();
      await expect(page).toHaveURL(/\/admin\/taxonomy\/character\/[0-9a-f-]{36}$/);
      const view = flow.locator("[data-ckui=node-view]");
      await expect(view.getByRole("heading", { name: `Cleo ${tag}` })).toBeVisible();
      const names = view.locator("[data-ckui=node-name]");
      await expect(names.filter({ hasText: `クレオ ${tag}` })).toHaveAttribute("data-kind", "name");

      // An alias, then it becomes the name (the old name stays as an alias).
      await view.getByRole("textbox", { name: "A name or alias" }).fill(`Cleopatra ${tag}`);
      await view.getByRole("button", { name: "Add name" }).click();
      const alias = names.filter({ hasText: `Cleopatra ${tag}` });
      await expect(alias).toHaveAttribute("data-kind", "alias");
      await alias.getByRole("button", { name: "Make it the name" }).click();
      await expect(alias).toHaveAttribute("data-kind", "name");
      await expect(names.filter({ hasText: `Cleo ${tag}` }).filter({ hasNotText: "Cleopatra" })).toHaveAttribute("data-kind", "alias");
      await expect(view.locator("[data-demo=host-field]")).toBeVisible();

      // A relation through the typeahead, seen from both sides.
      const relation = view.locator("[data-ckui=relation][data-relation=member_of]");
      await relation.getByRole("button", { name: "Add Series" }).click();
      await relation.getByRole("combobox", { name: "Series" }).fill(`Night Watch ${tag}`);
      await page.getByRole("option", { name: new RegExp(`Night Watch ${tag}`) }).click();
      const edge = relation.locator("[data-ckui=edge]", { hasText: `Night Watch ${tag}` });
      await expect(edge).toBeVisible();
      await edge.getByRole("button", { name: `Night Watch ${tag}`, exact: true }).click();
      await expect(page).toHaveURL(/\/admin\/taxonomy\/series\/[0-9a-f-]{36}$/);
      await expect(flow.getByRole("tab", { name: "Series" })).toHaveAttribute("aria-selected", "true");
      await expect(view.locator("[data-ckui=relation][data-relation='inverse:member_of']")).toContainText(`Cleopatra ${tag}`);

      // The series' cover.
      await view.locator("[data-ckui=cover-upload] input[type=file]").setInputFiles(path.resolve(import.meta.dirname, "..", "fixtures", "cover.jpg"));
      const crop = page.getByRole("dialog");
      await crop.getByRole("button", { name: "Save" }).click();
      await expect(crop).toBeHidden({ timeout: 30_000 });
      await expect(view.locator("[data-ckui=cover-upload] img")).toHaveAttribute("data-rendition", /^[0-9]+$/, { timeout: 60_000 });
      await page.screenshot({ path: `${dir}/taxonomy-manager-${mode}-${theme}-${info.project.name}.png`, fullPage: true });

      // Merge Colour into Color: its content moves over.
      await page.goto(admin(`/admin/taxonomy/tag/${colour.taxonomy_id}`, staff, { mode, theme }));
      await expect(view.locator("[data-ckui=node-content]")).toHaveCount(1, { timeout: 30_000 });
      await view.getByRole("button", { name: "Merge" }).click();
      await view.getByRole("combobox", { name: "Merge into" }).fill(`Color ${tag}`);
      await page.getByRole("option", { name: new RegExp(`Color ${tag}`) }).click();
      const confirm = page.getByRole("alertdialog");
      await expect(confirm.getByRole("button", { name: "Merge" })).toBeDisabled();
      await confirm.getByRole("textbox", { name: "Type merge to confirm" }).fill("merge");
      await confirm.getByRole("button", { name: "Merge" }).click();
      await expect(page).toHaveURL(new RegExp(`/admin/taxonomy/tag/${color.taxonomy_id}$`));
      await expect(view.locator("[data-ckui=node-content]")).toHaveCount(3);
      await expect(view.locator("[data-ckui=node-content]").first()).toContainText("gallery");
      await expect(names.filter({ hasText: `Colour ${tag}` })).toHaveAttribute("data-kind", "alias");

      // Delete and restore; the merged node reads as merged.
      await view.getByRole("button", { name: "Delete" }).click();
      await page.getByRole("alertdialog").getByRole("button", { name: "Delete" }).click();
      await expect(view).toHaveAttribute("data-state", "deleted");
      await view.getByRole("button", { name: "Restore" }).click();
      await expect(view).toHaveAttribute("data-state", "active");
      await page.goBack();
      await expect(view).toHaveAttribute("data-state", "merged");
      await expect(view.locator("[data-ckui=merged-into]")).toContainText(`Color ${tag}`);

      // The list's order and state are URL state; the merged node's names live on Color now.
      await page.goto(admin("/admin/taxonomy/tag", staff, { mode, theme, params: { q: tag } }));
      await expect(rows).toHaveText([new RegExp(`Color ${tag}`)], { timeout: 30_000 });
      await flow.getByRole("combobox", { name: "Order" }).click();
      await page.getByRole("option", { name: "Most used" }).click();
      await expect(page).toHaveURL(new RegExp(`/admin/taxonomy/tag\\?q=${tag}&sort=count$`));
      await flow.getByRole("combobox", { name: "State" }).click();
      await page.getByRole("option", { name: "Deleted" }).click();
      await expect(page).toHaveURL(/state=deleted/);
      await expect(flow.getByText("Nothing matches.")).toBeVisible();
      await page.goBack();
      await expect(rows).toHaveText([new RegExp(`Color ${tag}`)]);
    });
  }
}

test("a caller without taxonomy permission is told so", async ({ page }) => {
  await page.goto(admin("/admin/taxonomy", await h.user({ role: "moderator" }), { mode: "alone" }));
  await expect(page.locator("[data-ckui=taxonomy-manager] [data-ckui=no-access]")).toContainText("You don't have permission to manage the taxonomy.", { timeout: 30_000 });
});

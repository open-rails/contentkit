import { expect, test, type Page } from "@playwright/test";

// demo/social.html over the real content handlers (e2e/content-server.ts):
// needs CONTENTKIT_TEST_URL and CONTENTKIT_TEST_S3_* like `pnpm test:integration`.
test.skip(!process.env.CONTENTKIT_TEST_URL || !process.env.CONTENTKIT_TEST_S3_ENDPOINT, "needs the test PostgreSQL and MinIO");
test.describe.configure({ timeout: 90_000 });
// The first page builds and starts the server; writes under a loaded runner take a while.
test.use({ actionTimeout: 15_000 });
const slow = { timeout: 15_000 };

const dir = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";
const open = async (page: Page, query: string) => {
  await page.goto(`/social.html?${query}`);
  await expect(page.locator("[data-ckui=comments], [data-ckui=comment-moderation]").first()).toBeVisible({ timeout: 30_000 });
};

for (const theme of ["light", "dark"] as const) {
  test(`comments, reactions, favorites and a poll (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await open(page, `theme=${theme}`);
    const comments = page.locator("[data-ckui=comments]");
    await expect(comments.getByText("First! This trailer looks great.")).toBeVisible();
    await expect(comments.getByText("This comment was deleted.")).toBeVisible();
    await expect(comments.getByText("Awaiting review", { exact: true })).toBeVisible();
    await expect(comments.getByRole("button", { name: "Show more" })).toBeVisible();
    await page.screenshot({ path: `${dir}/social-${tag}.png`, fullPage: true });

    // Reply to the first comment; the reply opens under it.
    const first = comments.locator("[data-ckui=comment]", { hasText: "First! This trailer looks great." });
    await first.getByRole("button", { name: "Reply" }).click();
    await comments.getByRole("textbox", { name: "Write a reply…" }).fill("Same here!");
    await comments.getByRole("button", { name: "Reply", exact: true }).last().click();
    await expect(comments.getByText("Same here!")).toBeVisible();
    await expect(comments.getByRole("textbox", { name: "Write a reply…" })).toHaveCount(0, slow);
    await expect(comments.getByText("Agreed, the soundtrack at 1:20 is the best part.", { exact: false })).toBeVisible();

    // A new comment lands at the top; a like is pressed at once and stays.
    await comments.getByRole("textbox", { name: "Add a comment…" }).fill("Posting from Playwright");
    await comments.getByRole("button", { name: "Post", exact: true }).click();
    await expect(comments.locator("[data-ckui=comment]").first()).toContainText("Posting from Playwright", slow);
    const like = first.getByRole("button", { name: "Like", exact: true }).first();
    await like.click();
    await expect(like).toHaveAttribute("aria-pressed", "true");
    await expect(like).toContainText("2");

    const engagement = page.locator("[data-demo=engagement]");
    const itemLike = engagement.getByRole("button", { name: "Like", exact: true });
    await expect(itemLike).toContainText("1");
    await itemLike.click();
    await expect(itemLike).toHaveAttribute("aria-pressed", "true");
    await expect(itemLike).toContainText("2");
    await engagement.getByRole("button", { name: /Add to favorites/ }).click();
    await expect(engagement.getByRole("button", { name: /Remove from favorites/ })).toContainText("13");

    const poll = page.locator("[data-ckui=poll]");
    await poll.getByRole("button", { name: /Autumn 2026/ }).click();
    await expect(poll.getByRole("meter", { name: "Autumn 2026" })).toHaveAttribute("aria-valuenow", "25");
    await expect(poll.getByText("4 votes")).toBeVisible();
    await page.screenshot({ path: `${dir}/social-after-${tag}.png`, fullPage: true });
  });
}

test("signed out: comments, reactions and votes ask to sign in", async ({ page }) => {
  await open(page, "actor=");
  const comments = page.locator("[data-ckui=comments]");
  await comments.getByRole("button", { name: "Sign in to comment" }).click();
  await page.locator("[data-demo=engagement]").getByRole("button", { name: "Like", exact: true }).click();
  await page.locator("[data-ckui=poll]").getByRole("button", { name: /Spring 2026/ }).click();
  await expect(page.locator("[data-demo=sign-in]")).toHaveText("Sign-in requested (3)");
  await expect(page.locator("[data-ckui=poll]").getByRole("meter")).toHaveCount(0);
});

test("staff: the review queue, bans and the poll editor", async ({ page }, info) => {
  await open(page, "view=staff&actor=staff");
  const mod = page.locator("[data-ckui=comment-moderation]");
  await expect(mod.getByText("Selling cheap keys, DM me [hold]").first()).toBeVisible();
  await page.screenshot({ path: `${dir}/moderation-${info.project.name}.png`, fullPage: true });

  await mod.getByRole("tab", { name: "Review queue" }).click();
  const held = mod.locator("[data-ckui=held-item]", { hasText: "Selling cheap keys" }).first();
  await held.getByRole("button", { name: "Reject" }).click();
  await held.getByRole("textbox", { name: "Reason shown to the author (optional)" }).fill("Spam");
  await held.getByRole("button", { name: "Reject" }).click();
  await expect(held).toHaveCount(0);

  await mod.getByRole("tab", { name: "Bans" }).click();
  const bans = mod.locator("[data-ckui=comment-bans]");
  await bans.getByRole("textbox", { name: "User id" }).fill("troll");
  await bans.getByRole("button", { name: "Ban a user" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "30 days" }).click();
  await dialog.getByRole("textbox", { name: "Reason (optional)" }).fill("Harassment");
  await dialog.getByRole("button", { name: "Ban", exact: true }).click();
  const row = bans.locator("[data-ckui=comment-ban]", { hasText: "troll" });
  await expect(row).toContainText("Reason: Harassment");
  await page.screenshot({ path: `${dir}/bans-${info.project.name}.png`, fullPage: true });
  await row.getByRole("button", { name: "Lift ban" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Lift ban" }).click();
  await expect(row).toHaveCount(0);

  const create = page.locator("[data-demo=poll-editor]");
  await create.getByRole("textbox", { name: "Question" }).fill("Best opening song?");
  await create.getByRole("textbox", { name: "Option 1" }).fill("Opening A");
  await create.getByRole("textbox", { name: "Option 2" }).fill("Opening B");
  await create.getByRole("button", { name: "Create poll" }).click();
  await expect(create.getByRole("heading", { name: "Edit poll" })).toBeVisible();
  await expect(create.getByText("Poll created.")).toBeVisible();

  const edit = page.locator("[data-demo=poll-edit]");
  await edit.getByRole("button", { name: "Move up: Option 3" }).click();
  await expect(edit.getByRole("textbox", { name: "Option 2" })).toHaveValue("Autumn 2026");
  await page.screenshot({ path: `${dir}/poll-editor-${info.project.name}.png`, fullPage: true });
});

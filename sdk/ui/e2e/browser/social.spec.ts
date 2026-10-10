import { expect, test, type Page } from "@playwright/test";
import type { TestUser } from "../support/harness.ts";
import { seedSocial } from "../support/seed.ts";
import { harness, page as at, screenshots as dir } from "./pages.ts";

// demo/social.html over the real content module, seeded per test.
test.describe.configure({ timeout: 90_000 });
// Writes under a loaded runner take a while.
test.use({ actionTimeout: 15_000 });
const slow = { timeout: 15_000 };
const h = harness();

/** The social page as viewer (null: signed out) over a freshly seeded thread and poll. */
const open = async (page: Page, viewer: TestUser | null, params: Record<string, string> = {}) => {
  const { item, poll } = await seedSocial(h, { viewer, staff: params.view === "staff" });
  await page.goto(at("social.html", viewer, { ...params, item: item.id, poll: poll.id }));
  await expect(page.locator("[data-ckui=comments], [data-ckui=comment-moderation]").first()).toBeVisible({ timeout: 30_000 });
};

for (const theme of ["light", "dark"] as const) {
  test(`comments, reactions, favorites and a poll (${theme})`, async ({ page }, info) => {
    const tag = `${theme}-${info.project.name}`;
    await open(page, await h.user(), { theme });
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
    await expect(comments.getByRole("textbox", { name: "Write a reply…" })).toHaveCount(0, slow);
    await expect(comments.getByText("Same here!")).toBeVisible();
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
    await expect(engagement.getByRole("button", { name: /Add to favorites/ })).toContainText("2");
    await engagement.getByRole("button", { name: /Add to favorites/ }).click();
    await expect(engagement.getByRole("button", { name: /Remove from favorites/ })).toContainText("3");

    const poll = page.locator("[data-ckui=poll]");
    await poll.getByRole("button", { name: /Autumn 2026/ }).click();
    await expect(poll.getByRole("meter", { name: "Autumn 2026" })).toHaveAttribute("aria-valuenow", "25");
    await expect(poll.getByText("4 votes")).toBeVisible();
    await page.screenshot({ path: `${dir}/social-after-${tag}.png`, fullPage: true });
  });
}

test("signed out where the server takes nothing anonymous: comments, reactions and votes ask to sign in", async ({ page }) => {
  await open(page, null, { mount: "members" });
  const comments = page.locator("[data-ckui=comments]");
  await comments.getByRole("button", { name: "Sign in to comment" }).click();
  await expect(comments.getByRole("textbox")).toHaveCount(0);
  const like = page.locator("[data-demo=engagement]").getByRole("button", { name: "Like", exact: true });
  await expect(like).toHaveAttribute("title", "Sign in to react");
  await like.click();
  const poll = page.locator("[data-ckui=poll]");
  await expect(poll.getByText("Sign in to vote")).toBeVisible();
  await poll.getByRole("button", { name: /Spring 2026/ }).click();
  await expect(page.locator("[data-demo=sign-in]")).toHaveText("Sign-in requested (3)");
  await expect(poll.getByRole("meter")).toHaveCount(0);
});

test("signed out where the server takes anonymous interactions: comment under a name, react and vote", async ({ page }) => {
  await open(page, null);
  const comments = page.locator("[data-ckui=comments]");
  await expect(comments.getByRole("button", { name: "Sign in to comment" })).toHaveCount(0);
  await comments.getByRole("textbox", { name: "Name" }).fill("Guest");
  await comments.getByRole("textbox", { name: "Add a comment…" }).fill("Passing through");
  await comments.getByRole("button", { name: "Post", exact: true }).click();
  await expect(comments.locator("[data-ckui=comment]").first()).toContainText("Guest", slow);
  const like = page.locator("[data-demo=engagement]").getByRole("button", { name: "Like", exact: true });
  await like.click();
  await expect(like).toHaveAttribute("aria-pressed", "true");
  const poll = page.locator("[data-ckui=poll]");
  await poll.getByRole("button", { name: /Spring 2026/ }).click();
  await expect(poll.getByRole("meter", { name: "Spring 2026" })).toBeVisible();
  await expect(page.locator("[data-demo=sign-in]")).toHaveCount(0);
});

test("staff: the review queue, bans and the poll editor", async ({ page }, info) => {
  await open(page, await h.user({ role: "staff" }), { view: "staff" });
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
  const troll = await h.user();
  await bans.getByRole("textbox", { name: "User id" }).fill(troll.id);
  await bans.getByRole("button", { name: "Ban a user" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "30 days" }).click();
  await dialog.getByRole("textbox", { name: "Reason (optional)" }).fill("Harassment");
  await dialog.getByRole("button", { name: "Ban", exact: true }).click();
  const row = bans.locator("[data-ckui=comment-ban]", { hasText: troll.username });
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
  // Keyboard: lift the third option by its handle, move it up one, drop.
  await edit.getByRole("button", { name: "Reorder Autumn 2026" }).focus();
  await page.keyboard.press("Space");
  await page.waitForTimeout(80);
  await page.keyboard.press("ArrowUp");
  await page.waitForTimeout(80);
  await page.keyboard.press("Space");
  await expect(edit.getByRole("textbox", { name: "Option 2" })).toHaveValue("Autumn 2026");
  if (!info.project.use.isMobile) {
    // Pointer: drag the first option onto the second.
    await edit.getByRole("button", { name: "Reorder Spring 2026" }).dragTo(edit.getByRole("button", { name: "Reorder Autumn 2026" }), { steps: 12 });
    await expect(edit.getByRole("textbox", { name: "Option 1" })).toHaveValue("Autumn 2026");
    await expect(edit.getByRole("textbox", { name: "Option 2" })).toHaveValue("Spring 2026");
  }
  await page.screenshot({ path: `${dir}/poll-editor-${info.project.name}.png`, fullPage: true });
});

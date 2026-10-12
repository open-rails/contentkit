import { expect, test } from "@playwright/test";
import { harness, page as at, screenshots } from "./pages.ts";

const h = harness();
test.describe.configure({ timeout: 90_000 });

for (const theme of ["light", "dark"] as const) {
  test(`notification inbox and bell, arbitrary host events (${theme})`, async ({ page }, info) => {
    const user = await h.user();
    const other = await h.user();
    await h.notification(other, { kind: "app.private", data: { title: "Another person's update" } });
    await h.notification(user, { kind: "future.custom", data: { title: "Your export is ready", message: "Find the completed export in your downloads." } });
    await h.notification(user, { kind: "app.history", action_requested: true, data: { title: "Review request received", message: "Your review request was recorded on October 11." } });
    await h.notification(user, { kind: "app.review", status: "open", data: { title: "Review your draft", message: "Your teammate left feedback on the latest draft." } });
    await page.goto(at("notifications.html", user, { theme }));
    const inbox = page.locator("[data-ckui=notification-inbox]").first();
    await expect(inbox.getByText("Review your draft")).toBeVisible();
    await expect(inbox.getByText("Another person's update")).toHaveCount(0);
    await expect(inbox.getByText("Needs action")).toHaveCount(1);
    await expect(inbox.getByText("future.custom")).toHaveCount(0);
    await inbox.getByRole("button", { name: "Show more" }).click();
    await expect(inbox.getByText("Your export is ready")).toBeVisible();
    const task = inbox.locator("[data-ckui=notification-item]", { hasText: "Review your draft" });
    await expect(task.getByRole("button", { name: "Dismiss", exact: true })).toBeVisible();
    const general = inbox.locator("[data-ckui=notification-item]", { hasText: "Your export is ready" });
    await general.getByRole("button", { name: "Dismiss", exact: true }).click();
    await expect(general).toHaveCount(0);
    await inbox.getByRole("button", { name: "Archived", exact: true }).click();
    await inbox.getByRole("button", { name: "Restore to inbox" }).click();
    await inbox.getByRole("button", { name: "All", exact: true }).click();
    await inbox.getByRole("button", { name: "Show more" }).click();
    await expect(inbox.getByText("Your export is ready")).toBeVisible();
    await page.screenshot({ path: `${screenshots}/notifications-${theme}-${info.project.name}.png`, fullPage: true });
    for (const title of ["Review your draft", "Review request received", "Your export is ready"]) {
      const row = inbox.locator("[data-ckui=notification-item]", { hasText: title });
      await row.getByRole("button", { name: "Mark read", exact: true }).click();
      await expect(row).toHaveAttribute("data-unread", "false");
    }
    await expect(page.locator("[data-ckui=notification-unread]")).toHaveCount(0);
    const bell = page.locator("[data-ckui=notification-bell]").getByRole("button", { name: "Notifications", exact: true });
    await bell.focus();
    await page.keyboard.press("Enter");
    const popup = page.getByRole("dialog", { name: "Notifications", exact: true });
    await expect(popup.getByText("Review your draft")).toBeVisible();
    await page.screenshot({ path: `${screenshots}/notifications-bell-${theme}-${info.project.name}.png`, fullPage: true });
    await page.keyboard.press("Escape");
    await expect(popup).toHaveCount(0);
    await expect(bell).toBeFocused();
    await inbox.getByRole("button", { name: "Unread", exact: true }).click();
    await expect(inbox.getByText("You're all caught up.")).toBeVisible();
  });
}

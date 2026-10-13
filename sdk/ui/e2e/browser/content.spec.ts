import { expect, test } from "@playwright/test";
import { contentKit, seedContent } from "../support/seed.ts";
import { harness, page as demo, screenshots as dir } from "./pages.ts";

// Content pages in demo/content.tsx (listing, rails, search, a content's page)
// against the real ContentKit.
test.describe.configure({ timeout: 180_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const run = () => Math.random().toString(36).slice(2, 8);
const name = (n: string) => ({ language: "en", kind: "name" as const, name: n });

async function staffClient() {
  const staff = await h.user({ role: "staff" });
  return contentKit(h, staff);
}

for (const theme of ["light", "dark"] as const) {
  test(`listing: sort, trait, attribute and language filters in the URL (${theme})`, async ({ page }, info) => {
    const ck = await staffClient();
    const r = run();
    const [tag] = await ck.groups.create([{ kind: "tag", slug: `listing-${r}`, names: [name(`Listing ${r}`)] }]);
    const t = [tag!.id];
    await seedContent(ck, { kind: "comic", titles: { en: `Apple ${r}` }, attributes: { format: "manga" }, groups: t, versions: [{ languages: ["en"], traits: ["colored"] }] });
    await seedContent(ck, { kind: "comic", titles: { en: `Banana ${r}` }, attributes: { format: "doujin" }, groups: t, versions: [{ languages: ["en"] }] });
    await seedContent(ck, { kind: "comic", titles: { ja: `Cherry ${r}` }, access: { level: "tier", tiers: ["premium"] }, groups: t, versions: [{ languages: ["ja"] }] });

    await page.goto(demo("content.html", null, { view: "listing", kind: "comic", groups: tag!.id, theme }));
    const listing = page.locator("[data-ckui=content-listing]");
    const titles = listing.locator("[data-ckui=card-title]");
    // The kind's sort (newest) by default; a locked card names its tier.
    await expect(titles).toHaveText([`Cherry ${r} (Premium)`, `Banana ${r}`, `Apple ${r}`], { timeout: 30_000 });
    await expect(listing.locator("[data-ckui=content-card][data-locked]")).toHaveCount(1);
    await page.screenshot({ path: `${dir}/content-listing-${theme}-${info.project.name}.png`, fullPage: true });

    await listing.getByRole("combobox", { name: "Sort" }).click();
    await page.getByRole("option", { name: "Title" }).click();
    await expect(page).toHaveURL(/sort=title/);
    await expect(titles).toHaveText([`Apple ${r}`, `Banana ${r}`, `Cherry ${r} (Premium)`]);

    await listing.getByRole("button", { name: "Colored" }).click();
    await expect(page).toHaveURL(/traits=colored/);
    await expect(titles).toHaveText([`Apple ${r}`]);
    await expect(listing.getByRole("button", { name: "Colored" })).toHaveAttribute("aria-pressed", "true");
    await listing.getByRole("button", { name: "Clear filters" }).click();
    await expect(titles).toHaveText([`Apple ${r}`, `Banana ${r}`, `Cherry ${r} (Premium)`]);

    await listing.getByRole("combobox", { name: "Format" }).click();
    await page.getByRole("option", { name: "Doujin" }).click();
    await expect(page).toHaveURL(/attribute=format%3Adoujin/);
    await expect(titles).toHaveText([`Banana ${r}`]);
    await page.goBack();
    await expect(titles).toHaveText([`Apple ${r}`, `Banana ${r}`, `Cherry ${r} (Premium)`]);

    await listing.getByRole("combobox", { name: "Language" }).click();
    await page.getByRole("option", { name: "Japanese" }).click();
    await expect(page).toHaveURL(/language=ja/);
    await expect(titles).toHaveText([`Cherry ${r} (Premium)`]);
  });
}

test("rails: a featured group in its order and windows, a list, no continue watching without the signal plane", async ({ page }, info) => {
  const ck = await staffClient();
  const r = run();
  const [tag, rail] = await ck.groups.create([
    { kind: "tag", slug: `rails-${r}`, names: [name(`Rails ${r}`)] },
    { kind: "rail", slug: `featured-${r}`, names: [name(`Featured ${r}`)] },
  ]);
  const t = [tag!.id];
  const one = await seedContent(ck, { kind: "comic", titles: { en: `One ${r}` }, groups: t, versions: [{ languages: ["en"] }] });
  const two = await seedContent(ck, { kind: "comic", titles: { en: `Two ${r}` }, groups: t, versions: [{ languages: ["en"] }] });
  const later = await seedContent(ck, { kind: "comic", titles: { en: `Later ${r}` }, groups: t, versions: [{ languages: ["en"] }] });
  const tomorrow = new Date(Date.now() + 86_400_000).toISOString();
  await ck.groups.addMembers(rail!.id, [{ content_id: two.id }, { content_id: one.id, active_until: tomorrow }, { content_id: later.id, active_from: tomorrow }]);

  await page.goto(demo("content.html", null, { view: "rails", featured: rail!.id, groups: tag!.id }));
  const featured = page.locator("[data-ckui=content-rail][data-surface=featured]");
  await expect(featured.getByRole("heading", { name: "Featured" })).toBeVisible({ timeout: 30_000 });
  await expect(featured.locator("[data-ckui=card-title]")).toHaveText([`Two ${r}`, `One ${r}`]);
  const newest = page.locator("[data-ckui=content-rail][data-surface=list]");
  await expect(newest.locator("[data-ckui=card-title]")).toHaveText([`Later ${r}`, `Two ${r}`, `One ${r}`]);
  await expect(page.locator("[data-ckui=content-rail]")).toHaveCount(2);
  await page.screenshot({ path: `${dir}/content-rails-${info.project.name}.png`, fullPage: true });
});

test("content page: version picker, paywall checkout with the tier, the player's version menu", async ({ page }, info) => {
  const ck = await staffClient();
  const r = run();
  const c = await seedContent(ck, {
    kind: "comic",
    titles: { en: `Atlas ${r}` },
    access: { level: "tier", tiers: ["premium"] },
    versions: [{ languages: ["en"] }, { languages: ["en"], traits: ["colored"], access: { level: "public" } }, { languages: ["ja"] }],
  });
  const [plain, colored, ja] = c.versions;

  await page.goto(demo("content.html", null, { view: "detail", id: c.id }));
  await expect(page.locator("[data-demo=title]")).toHaveText(`Atlas ${r}`, { timeout: 30_000 });
  const picker = page.locator("[data-ckui=version-picker]");
  await expect(picker.locator("[data-ckui=version-option]")).toHaveText(["English", "Colored"]);
  await expect(page.locator("[data-demo=player-menu] li")).toHaveText(["Language: English | Japanese", "Version: English (locked) | Colored"]);

  await picker.getByRole("radio", { name: "Colored" }).click();
  await expect(page.locator("[data-demo=version]")).toHaveText(colored!.id);
  await expect(page.locator("[data-ckui=paywall]")).toHaveCount(0);

  await picker.getByRole("radio", { name: "English" }).click();
  await expect(page.locator("[data-demo=version]")).toHaveText(plain!.id);
  const paywall = page.locator("[data-ckui=paywall]");
  await expect(paywall).toBeVisible();
  await page.screenshot({ path: `${dir}/content-page-${info.project.name}.png`, fullPage: true });
  await paywall.getByRole("button", { name: /Premium/ }).click();
  expect(await page.evaluate(() => (window as unknown as { events: { name: string; key: string }[] }).events)).toEqual([{ name: "checkout", key: "premium" }]);

  await picker.getByRole("combobox", { name: "Language" }).click();
  await page.getByRole("option", { name: "Japanese" }).click();
  await expect(page.locator("[data-demo=version]")).toHaveText(ja!.id);
  await expect(page.locator("[data-demo=player-menu] li")).toHaveText(["Language: English | Japanese"]);
});

test("search page: suggestions while typing, results on Enter", async ({ page }) => {
  const ck = await staffClient();
  const r = run();
  const word = `qz${r}`;
  const c = await seedContent(ck, { kind: "comic", titles: { en: `Kiwi ${word}` }, versions: [{ languages: ["en"] }] });
  // Keyword documents trail writes by the search sync job.
  await expect.poll(async () => (await ck.discovery.search({ q: word })).contents.map((x) => x.id), { timeout: 90_000, intervals: [1000] }).toEqual([c.id]);

  await page.goto(demo("content.html", null, { view: "search" }));
  const box = page.locator("[data-ckui=search-box]");
  await box.getByRole("combobox").fill(word);
  await expect(box.locator("[data-ckui=suggestion]")).toHaveText([new RegExp(`Kiwi ${word}`)], { timeout: 30_000 });
  await box.getByRole("combobox").press("Enter");
  await expect(page).toHaveURL(new RegExp(`q=${word}`));
  await expect(page.locator("[data-ckui=content-grid] [data-ckui=card-title]")).toHaveText([`Kiwi ${word}`], { timeout: 30_000 });
});

import path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { contentKit } from "../support/seed.ts";
import { harness, page as at, screenshots as dir } from "./pages.ts";

// demo/post.html over the real content module: a post body stores image
// references, so its images show through publish, hide and show.
test.describe.configure({ timeout: 150_000 });
test.use({ actionTimeout: 15_000 });
const h = harness();
const small = path.resolve(import.meta.dirname, "..", "fixtures", "small.png");

/** Whether every image under selector has loaded. */
const loaded = (p: Page, selector: string) =>
  p.locator(`${selector} img`).evaluateAll((imgs) => imgs.length > 0 && imgs.every((i) => (i as HTMLImageElement).complete && (i as HTMLImageElement).naturalWidth > 0));

test("a post's body images show through publish, hide and show", async ({ page, browser }, info) => {
  const editor = await h.user({ role: "editor" });
  const ck = contentKit(h, editor);
  const post = await ck.posts.create({ title: "Pictures", body: "<p>Hello</p>", language: "en", is_draft: true });

  // The editor places an image: it shows from the file, and the body stores its reference.
  await page.goto(at("post.html", editor, { post: post.id, view: "editor" }));
  const field = page.locator("[data-demo=body]");
  await expect(field).toContainText("Hello", { timeout: 30_000 });
  await page.getByLabel("Add image").setInputFiles(small);
  await expect.poll(() => loaded(page, "[data-demo=body]"), { timeout: 30_000 }).toBe(true);
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(async () => (await ck.posts.get(post.id)).body, { timeout: 15_000 }).toMatch(/<img src="contentkit:i-[0-9a-f-]{36}"/);
  await expect.poll(() => loaded(page, "[data-demo=body]"), { timeout: 30_000 }).toBe(true);

  // Signed out, the draft does not exist.
  const reader = await browser.newPage();
  await reader.goto(at("post.html", null, { post: post.id }));
  await expect(reader.locator("[data-demo=missing]")).toHaveText("not_found", { timeout: 30_000 });

  /** The reader's image source once the published post's images load, other than not. */
  const shown = async (not?: string | null) => {
    let src: string | null = null;
    await expect
      .poll(
        async () => {
          await reader.reload();
          if (!(await reader.locator("[data-demo=post]").isVisible({ timeout: 10_000 }).catch(() => false))) return false;
          src = await reader.locator("[data-demo=post] img").first().getAttribute("src");
          return src !== not && (await loaded(reader, "[data-demo=post]"));
        },
        { timeout: 90_000, intervals: [1000] },
      )
      .toBe(true);
    return src;
  };
  await page.getByRole("button", { name: "Publish" }).click();
  await expect(page.locator("[data-demo=state]")).toHaveText("Published");
  const first = await shown();
  expect(first).toMatch(/\/public\/i-[0-9a-f-]{36}-[0-9a-f-]{36}\.webp$/);
  await reader.screenshot({ path: `${dir}/post-${info.project.name}.png`, fullPage: true });

  // Hidden and shown again, the image publishes a new generation; the stored reference follows it.
  await page.getByRole("button", { name: "Unpublish" }).click();
  await expect(page.locator("[data-demo=state]")).toHaveText("Draft");
  await page.getByRole("button", { name: "Publish" }).click();
  await expect(page.locator("[data-demo=state]")).toHaveText("Published");
  const again = await shown(first);
  expect(again).toMatch(/\/public\/i-[0-9a-f-]{36}-[0-9a-f-]{36}\.webp$/);
  expect((await reader.request.get(first!)).status()).toBe(404);
  await reader.close();
});

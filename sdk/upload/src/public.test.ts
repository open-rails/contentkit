import { expect, it } from "vitest";
import { fill, publicRenditions, publicURL, srcSet } from "./public.js";

it("builds public URLs and srcsets like media.PublicURL and media.SrcSet", () => {
  const id = "0192f000-0000-7000-8000-000000000001";
  expect(publicURL("https://media.doujins.ai/", "doujins", "gallery", id, "cover-460.webp")).toBe(`https://media.doujins.ai/v1/doujins/gallery/${id}/public/cover-460.webp`);
  expect(srcSet("https://m", "accounts", "user", id, "avatar-{w}.webp", [128, 64])).toBe(
    `https://m/v1/accounts/user/${id}/public/avatar-128.webp 128w, https://m/v1/accounts/user/${id}/public/avatar-64.webp 64w`,
  );
  expect(fill("{name}-{w}.{x}", { name: "i-1", w: 3 })).toBe("i-1-3.");
});

it("lists a preset's files narrowest first, with heights at its aspect", () => {
  const image = { base: "https://m", namespace: "doujins", kind: "gallery", id: "1", to: "cover-{w}.webp", widths: [460, 230], aspect: "46:65" };
  expect(publicRenditions(image)).toEqual([
    { w: 230, h: 325, url: "https://m/v1/doujins/gallery/1/public/cover-230.webp" },
    { w: 460, h: 650, url: "https://m/v1/doujins/gallery/1/public/cover-460.webp" },
  ]);
  expect(publicRenditions({ ...image, aspect: "" })[0]!.h).toBeUndefined();
  expect(publicRenditions(null)).toEqual([]);
});

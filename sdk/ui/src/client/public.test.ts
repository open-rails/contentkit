import { expect, it } from "vitest";
import { fill, publicRenditions, publicURL, srcSet } from "./public.js";

it("uses published names and actual rendition widths in public URLs and srcsets", () => {
  const id = "0192f000-0000-7000-8000-000000000001";
  expect(publicURL("https://media.doujins.ai/", "doujins", "gallery", id, "cover-460.webp")).toBe(`https://media.doujins.ai/v1/doujins/gallery/${id}/public/cover-460.webp`);
  expect(srcSet([{ w: 100, h: 100, url: "https://m/avatar-128-generation.webp" }, { w: 64, h: 64, url: "https://m/avatar-64-generation.webp" }])).toBe(
    "https://m/avatar-64-generation.webp 64w, https://m/avatar-128-generation.webp 100w",
  );
  expect(fill("{name}-{w}.{x}", { name: "i-1", w: 3 })).toBe("i-1-3.");
  // Presets wider than the source can produce the same encoded width.
  expect(srcSet([
    { w: 100, url: "https://m/avatar-128-generation.webp" },
    { w: 100, url: "https://m/avatar-256-generation.webp" },
  ])).toBe("https://m/avatar-128-generation.webp 100w");
});

it("lists the returned renditions without deriving names or inflating dimensions", () => {
  const image = { preset: "cover", aspect: "46:65", renditions: [
    { w: 400, h: 565, url: "https://m/cover-460-generation.webp" },
    { w: 230, h: 325, url: "https://m/cover-230-generation.webp" },
  ] };
  expect(publicRenditions(image)).toEqual([
    { w: 230, h: 325, url: "https://m/cover-230-generation.webp" },
    { w: 400, h: 565, url: "https://m/cover-460-generation.webp" },
  ]);
  expect(publicRenditions({ ...image, aspect: "" })[0]!.h).toBe(325);
  expect(publicRenditions(null)).toEqual([]);
});

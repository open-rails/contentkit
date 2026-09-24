import { expect, it } from "vitest";
import { largestOutput, manifestAspect, slotSources } from "./srcset.js";
import type { SlotManifest } from "./wire.gen.js";

const m: SlotManifest = {
  aspect: "3:1",
  pending: false,
  outputs: [
    { name: "cover_3000", w: 3000, h: 1000, url: "https://cdn/cover_3000.webp" },
    { name: "cover_1500", w: 1500, h: 500, url: "https://cdn/cover_1500.webp" },
  ],
};

it("builds srcset smallest first with a fallback src", () => {
  expect(slotSources(m)).toEqual({
    src: "https://cdn/cover_1500.webp",
    srcSet: "https://cdn/cover_1500.webp 1500w, https://cdn/cover_3000.webp 3000w",
    width: 1500,
    height: 500,
  });
  expect(slotSources(m, 2000).src).toBe("https://cdn/cover_3000.webp");
});

it("is empty for an empty slot and reads aspect and the largest output", () => {
  expect(slotSources({ aspect: "1:1", outputs: [], pending: false })).toEqual({});
  expect(slotSources(null)).toEqual({});
  expect(manifestAspect(m)).toBe("3:1");
  expect(manifestAspect(undefined, "2:1")).toBe("2:1");
  expect(manifestAspect({ aspect: "", pending: false, outputs: [{ name: "p", w: 480, h: 853, url: "u" }] })).toBe("480:853");
  expect(largestOutput(m)?.w).toBe(3000);
});

it("knows when there is an original to re-edit", async () => {
  const { hasOriginal } = await import("./srcset.js");
  expect(hasOriginal({ aspect: "3:1", outputs: [], pending: false })).toBe(false);
  expect(hasOriginal({ aspect: "3:1", outputs: [], pending: true })).toBe(true);
  expect(hasOriginal(m)).toBe(true);
});

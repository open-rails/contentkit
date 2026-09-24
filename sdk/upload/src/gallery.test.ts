import { expect, it } from "vitest";
import { formatDuration, galleryItems, stageAspect } from "./gallery.js";
import type { FileInfo, ReadResult } from "./wire.gen.js";

const read = (access: string, files: FileInfo[]): ReadResult => ({ access, total: files.length, preview_limit: 0, offset: 0, limit: 50, expires: 0, files });
const img = (index: number, o: Partial<FileInfo> = {}): FileInfo => ({ index, name: `${index}.png`, type: "image/png", w: 800, h: 600, url: `https://m/${index}`, ...o });

it("full access: every file in order, teaser hidden", () => {
  const items = galleryItems(read("full", [img(0, { teaser: true }), img(1), { index: 2, name: "v.mp4", type: "video/mp4", w: 1080, h: 1920, hls: true }]));
  expect(items.map((i) => i.kind)).toEqual(["image", "video"]);
  expect(items[1]!.aspect).toBeCloseTo(0.5625);
});

it("no access: the teaser sits behind one locked item and nothing locked leaks", () => {
  const files = [img(0, { teaser: true, url: "https://m/blurred" }), { index: 1, type: "image/png", locked: true }, { index: 2, type: "video/mp4", locked: true }];
  const items = galleryItems(read("none", files));
  expect(items).toEqual([{ kind: "locked", key: "locked", count: 2, videos: 1, teaser: files[0], aspect: 800 / 600 }]);
});

it("preview access: allowed files, then the locked rest", () => {
  const items = galleryItems(read("preview", [img(0), img(1), { index: 2, type: "image/png", locked: true }]));
  expect(items.map((i) => i.kind)).toEqual(["image", "image", "locked"]);
  expect(items[2]).toMatchObject({ count: 1, teaser: undefined });
});

it("stage aspect is the current item's native aspect; durations format", () => {
  expect(stageAspect([])).toBe(1);
  const items = galleryItems(read("full", [img(0, { w: 300, h: 1000 }), img(1, { w: 3000, h: 1000 })]));
  expect(stageAspect(items)).toBeCloseTo(0.3);
  expect(stageAspect(items, 1)).toBe(3);
  expect(formatDuration(42.4)).toBe("0:42");
  expect(formatDuration(725)).toBe("12:05");
  expect(formatDuration(3729)).toBe("1:02:09");
});

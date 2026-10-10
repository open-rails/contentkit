import { expect, it } from "vitest";
import { formatDuration, galleryItems, stageAspect } from "./gallery.js";
import type { Access, FileInfo, ReadResult } from "./generated/wire.js";

const read = (access: Access, files: FileInfo[], hls?: string[]): ReadResult => ({ access, total: files.length, offset: 0, limit: 50, expires: 0, files, hls });
const img = (n: number, o: Partial<FileInfo> = {}): FileInfo => ({ path: `low-res/${n}.webp`, type: "image/webp", w: 800, h: 600, url: `https://m/${n}`, ...o });
const ladder = (dir: string, o: Partial<FileInfo> = {}): FileInfo[] => [
  { path: `${dir}480-h264.mp4`, type: "video/mp4", w: 270, h: 480, url: "u", ...o },
  { path: `${dir}1080-h264.mp4`, type: "video/mp4", w: 1080, h: 1920, dur: 42, url: "u", ...o },
  { path: `${dir}audio-a1.mp4`, type: "audio/mp4", url: "u", ...o },
  { path: `${dir}sprite.jpg`, type: "image/jpeg", url: "u", ...o },
];

it("full access: images in order, one item per HLS ladder, no previews; downloads and subtitles are not items", () => {
  const files = [img(1), ...ladder("hls/"), { path: "video/source-1080p.mp4", type: "video/mp4", url: "u" }, { path: "subs/en.vtt", type: "text/vtt", url: "u" }];
  const items = galleryItems({ ...read("full", files, ["hls/"]), previews: ["https://m/public/preview-1.webp"] });
  expect(items.map((i) => [i.kind, i.key])).toEqual([["image", "low-res/1.webp"], ["video", "hls/"]]);
  expect(items[1]).toMatchObject({ dir: "hls/", file: { path: "hls/1080-h264.mp4", dur: 42 } });
  expect(items[1]!.aspect).toBeCloseTo(0.5625);
});

it("an audio-only ladder plays its M4A", () => {
  const files: FileInfo[] = [
    { path: "listen/song/audio.mp4", type: "audio/mp4", dur: 125, url: "track" },
    { path: "listen/song/audio.m4a", type: "audio/mp4", dur: 125, url: "m4a", download: "Song.m4a" },
  ];
  expect(galleryItems(read("full", files, ["listen/song/"]))).toEqual([{ kind: "audio", key: "listen/song/", dir: "listen/song/", file: files[1], aspect: 3 }]);
});

const lockedImg = (n: number): FileInfo => ({ path: `low-res/${n}.webp`, type: "image/webp", size: 1000, locked: true });

it("no access: one locked item, a locked ladder counts once, and nothing locked leaks", () => {
  const files = [lockedImg(0), lockedImg(1), ...ladder("hls/", { url: undefined, locked: true })];
  expect(galleryItems(read("none", files))).toEqual([{ kind: "locked", key: "locked", count: 3, videos: 1, backdrop: undefined, aspect: 1 }]);
});

it("no access with previews: the public previews, then the locked rest behind the last preview", () => {
  const previews = ["https://m/public/preview-1.webp", "https://m/public/preview-2.webp"];
  const items = galleryItems({ ...read("none", [lockedImg(0), lockedImg(1), lockedImg(2)]), previews });
  expect(items).toEqual([
    { kind: "image", key: "preview/1", file: { path: "preview/1", type: "image/webp", url: previews[0] }, aspect: 1 },
    { kind: "image", key: "preview/2", file: { path: "preview/2", type: "image/webp", url: previews[1] }, aspect: 1 },
    { kind: "locked", key: "locked", count: 1, videos: 0, backdrop: previews[1], aspect: 1 },
  ]);
  // Every page previewed: nothing is left to lock.
  expect(galleryItems({ ...read("none", [lockedImg(0), lockedImg(1)]), previews }).map((i) => i.kind)).toEqual(["image", "image"]);
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

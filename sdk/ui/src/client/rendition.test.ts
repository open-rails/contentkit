import { expect, it } from "vitest";
import { densityFor, pickRendition } from "./rendition.js";

const outs = [1920, 640, 1280, 960, 2560].map((w) => ({ w, h: Math.round((w * 9) / 16), url: `https://cdn/${w}.webp` }));

it("densityFor preserves actual devicePixelRatio within 1–3× by default", () => {
  expect(densityFor(undefined, 1)).toBe(1);
  expect(densityFor(undefined, 2.625)).toBe(2.625);
  expect(densityFor(undefined, 4)).toBe(3);
  expect(densityFor([1, 1.5], 2)).toBe(1.5);
});

it("gallery cards use thumbnail-sized renditions on desktop and retain phone density", () => {
  const covers = [230, 460, 690, 920, 1380].map((w) => ({ w, url: `https://cdn/${w}.webp` }));
  expect(pickRendition(covers, 336, densityFor(undefined, 1))?.w).toBe(460);
  expect(pickRendition(covers, 336, densityFor(undefined, 2))?.w).toBe(690);
  expect(pickRendition(covers, 175, densityFor(undefined, 3))?.w).toBe(690);
});

it("pickRendition takes the narrowest ≥ CSS width × density, else the widest", () => {
  expect(pickRendition(outs, 250, 2)?.w).toBe(640);
  expect(pickRendition(outs, 400, 2)?.w).toBe(960);
  expect(pickRendition(outs, 400, 3)?.w).toBe(1280);
  expect(pickRendition(outs, 600, 3)?.w).toBe(1920);
  expect(pickRendition(outs, 2000, 3)?.w).toBe(2560);
  expect(pickRendition([], 100, 2)).toBeUndefined();
});

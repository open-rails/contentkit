import { expect, it } from "vitest";
import { centeredCrop, constrainCrop, editOf, rotation, toOriginal } from "./crop.js";

const src = { width: 400, height: 200 };

it("keeps crops inside the source in whole pixels", () => {
  expect(constrainCrop({ x: -5, y: 150.4, w: 500, h: 99.6 }, src)).toEqual({ x: 0, y: 100, w: 400, h: 100 });
  expect(constrainCrop({ x: 390, y: 0, w: 50, h: 0 }, src)).toEqual({ x: 350, y: 0, w: 50, h: 1 });
});

it("derives the height from the width at the aspect, as the server does", () => {
  // 1:2 covers from a 400×200 page: at most 100 wide.
  expect(constrainCrop({ x: 200, y: 0, w: 100, h: 3 }, src, 0.5)).toEqual({ x: 200, y: 0, w: 100, h: 200 });
  expect(constrainCrop({ x: 0, y: 0, w: 300, h: 0 }, src, 0.5)).toEqual({ x: 0, y: 0, w: 100, h: 200 });
  // Rotated a quarter turn the crop is the transpose: 200 wide, 100 high.
  expect(constrainCrop({ x: 100, y: 0, w: 200, h: 0 }, src, 0.5, 90)).toEqual({ x: 100, y: 0, w: 200, h: 100 });
  // Odd aspects never round past the source.
  for (let w = 1; w <= 400; w++) {
    const c = constrainCrop({ x: 0, y: 0, w, h: 0 }, { width: 400, height: 133 }, 3.01);
    expect(c.h).toBe(Math.round(c.w / 3.01));
    expect(c.h).toBeLessThanOrEqual(133);
  }
});

it("maps display pixels to original pixels and builds edits", () => {
  expect(toOriginal({ x: 10, y: 5, w: 50, h: 25 }, { width: 100, height: 50 }, src)).toEqual({ x: 40, y: 20, w: 200, h: 100 });
  expect(centeredCrop(src, 1)).toEqual({ x: 100, y: 0, w: 200, h: 200 });
  expect(editOf({ x: 0, y: 0, w: 400, h: 200 }, src)).toBeNull();
  expect(editOf(null, src, 90)).toEqual({ rotate: 90 });
  expect(editOf({ x: 1, y: 0, w: 10, h: 10 }, src, 0)).toEqual({ crop: { x: 1, y: 0, w: 10, h: 10 } });
  expect([rotation(-90), rotation(450), rotation(180)]).toEqual([270, 90, 180]);
});

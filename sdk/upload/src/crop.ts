import type { Crop, Edit } from "./wire.gen.js";

/** Clockwise degrees. */
export type Rotation = 0 | 90 | 180 | 270;

export interface Size {
  width: number;
  height: number;
}

/**
 * Fits rect (original pixels) inside source as whole pixels. With aspect (the
 * edited image's width/height; with a 90/270 rotation the crop is its
 * transpose) the height follows the width exactly as the server derives it.
 */
export function constrainCrop(rect: Crop, source: Size, aspect?: number, rotate: Rotation = 0): Crop {
  const W = Math.max(1, Math.round(source.width));
  const H = Math.max(1, Math.round(source.height));
  let w = clamp(Math.round(rect.w), 1, W);
  let h = clamp(Math.round(rect.h), 1, H);
  if (aspect && aspect > 0) {
    const r = rotate === 90 || rotate === 270 ? 1 / aspect : aspect; // crop width / height
    h = Math.round(w / r);
    if (h > H) w = Math.round(H * r);
    w = clamp(w, 1, W);
    h = Math.round(w / r);
    while (h > H && w > 1) h = Math.round(--w / r);
    while (h < 1 && w < W) h = Math.round(++w / r);
    h = Math.max(1, h);
  }
  return { x: clamp(Math.round(rect.x), 0, W - w), y: clamp(Math.round(rect.y), 0, H - h), w, h };
}

/** Maps a rect on the image shown at display size (unrotated) to original pixels. */
export function toOriginal(rect: Crop, display: Size, source: Size): Crop {
  const sx = source.width / display.width;
  const sy = source.height / display.height;
  return { x: rect.x * sx, y: rect.y * sy, w: rect.w * sx, h: rect.h * sy };
}

/** The largest centred crop (at aspect, if any). */
export function centeredCrop(source: Size, aspect?: number, rotate: Rotation = 0): Crop {
  const c = constrainCrop({ x: 0, y: 0, w: source.width, h: source.height }, source, aspect, rotate);
  return { ...c, x: Math.floor((source.width - c.w) / 2), y: Math.floor((source.height - c.h) / 2) };
}

/** The edit for a crop and rotation; null when it changes nothing. */
export function editOf(crop: Crop | null, source: Size, rotate: Rotation = 0): Edit | null {
  const full = !crop || (crop.x === 0 && crop.y === 0 && crop.w === source.width && crop.h === source.height);
  if (full && rotate === 0) return null;
  return { ...(full ? {} : { crop }), ...(rotate ? { rotate } : {}) };
}

export function rotation(deg: number): Rotation {
  return ((((Math.round(deg / 90) * 90) % 360) + 360) % 360) as Rotation;
}

function clamp(v: number, lo: number, hi: number) {
  return Math.min(Math.max(v, lo), hi);
}

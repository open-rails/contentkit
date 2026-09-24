import { aspectOf, ratio, type AspectRatio } from "./aspect.js";
import type { SlotImage, SlotManifest } from "./wire.gen.js";

export interface SlotSources {
  src?: string;
  srcSet?: string;
  width?: number;
  height?: number;
}

/**
 * `srcset` from a slot manifest's outputs (fixed URLs, revalidated by
 * ETag); src is the smallest output at least fallbackWidth wide.
 */
export function slotSources(m: SlotManifest | null | undefined, fallbackWidth = 512): SlotSources {
  const outs = [...(m?.outputs ?? [])].filter((o) => o.url && o.w > 0).sort((a, b) => a.w - b.w);
  if (outs.length === 0) return {};
  const fallback = outs.find((o) => o.w >= fallbackWidth) ?? outs.at(-1)!;
  return { src: fallback.url, srcSet: outs.map((o) => `${o.url} ${o.w}w`).join(", "), width: fallback.w, height: fallback.h };
}

/** The manifest's "W:H" (a native slot's from its widest output), or fallback. */
export function manifestAspect(m: SlotManifest | null | undefined, fallback: AspectRatio = "1:1"): AspectRatio {
  if (m && ratio(m.aspect)) return m.aspect;
  const o = [...(m?.outputs ?? [])].reverse().find((o) => o.w > 0 && o.h > 0);
  return o ? aspectOf(o.w, o.h) : fallback;
}

/** Widest rendered output; with nothing rendered, undefined. */
export function largestOutput(m: SlotManifest | null | undefined): SlotImage | undefined {
  return m?.outputs.reduce<SlotImage | undefined>((a, o) => (!a || o.w > a.w ? o : a), undefined);
}

/** The slot has a committed original to re-edit (dims arrive only with the first encode). */
export function hasOriginal(m: SlotManifest | null | undefined): boolean {
  return !!m && (m.pending || !!m.dims || m.outputs.length > 0);
}

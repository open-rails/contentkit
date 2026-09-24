import type { SlotImage, SlotManifest } from "./wire.gen.js";

export interface SlotSources {
  src?: string;
  srcSet?: string;
  width?: number;
  height?: number;
}

/**
 * `srcset` from a slot manifest's outputs (URLs are versioned, so cache
 * freely); src is the smallest output at least fallbackWidth wide.
 */
export function slotSources(m: SlotManifest | null | undefined, fallbackWidth = 512): SlotSources {
  const outs = [...(m?.outputs ?? [])].filter((o) => o.url && o.w > 0).sort((a, b) => a.w - b.w);
  if (outs.length === 0) return {};
  const fallback = outs.find((o) => o.w >= fallbackWidth) ?? outs.at(-1)!;
  return { src: fallback.url, srcSet: outs.map((o) => `${o.url} ${o.w}w`).join(", "), width: fallback.w, height: fallback.h };
}

/** The manifest's width / height, or fallback. */
export function manifestAspect(m: SlotManifest | null | undefined, fallback = 1): number {
  return m && m.aspect > 0 ? m.aspect : fallback;
}

/** Widest rendered output; with nothing rendered, undefined. */
export function largestOutput(m: SlotManifest | null | undefined): SlotImage | undefined {
  return m?.outputs.reduce<SlotImage | undefined>((a, o) => (!a || o.w > a.w ? o : a), undefined);
}

/** The slot has a committed original to re-edit (dims arrive only with the first encode). */
export function hasOriginal(m: SlotManifest | null | undefined): boolean {
  return !!m && (m.pending || !!m.dims || m.outputs.length > 0);
}

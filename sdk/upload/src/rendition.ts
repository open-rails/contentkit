/** One rendered size of an image: a slot output, a cover, a variant. */
export interface Rendition {
  w: number;
  h: number;
  url: string;
}

/** Device-pixel density range renditions target: even 1× screens get 2×, 3× phones 3×. */
export type DensityRange = readonly [min: number, max: number];

export const DEFAULT_DENSITY: DensityRange = [2, 3];

/** devicePixelRatio clamped into range. */
export function densityFor(range: DensityRange = DEFAULT_DENSITY, dpr = typeof devicePixelRatio === "number" ? devicePixelRatio : 1): number {
  return Math.min(range[1], Math.max(range[0], dpr || 1));
}

/** Renditions with a URL and width, narrowest first. */
export function sortRenditions<T extends Rendition>(outputs: readonly T[] | null | undefined): T[] {
  return [...(outputs ?? [])].filter((o) => o.url && o.w > 0).sort((a, b) => a.w - b.w);
}

/**
 * The narrowest rendition at least cssWidth × density device pixels wide
 * (the widest when none is), e.g. 400 CSS px at 2× picks ≥ 800 px.
 */
export function pickRendition<T extends Rendition>(outputs: readonly T[] | null | undefined, cssWidth: number, density = densityFor()): T | undefined {
  const sorted = sortRenditions(outputs);
  const want = cssWidth * density;
  return sorted.find((o) => o.w >= want) ?? sorted.at(-1);
}

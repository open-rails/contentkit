/** A width:height ratio as the API writes it: "3:1", "9:16". "" is native (the source's own shape). */
export type AspectRatio = string;

/** The ratio's value (width / height) for CSS `aspect-ratio` and crop maths; undefined for "" or an invalid ratio. */
export function ratio(a: AspectRatio | null | undefined): number | undefined {
  const m = /^\s*(\d+)\s*:\s*(\d+)\s*$/.exec(a ?? "");
  if (!m) return undefined;
  const w = Number(m[1]);
  const h = Number(m[2]);
  return w > 0 && h > 0 ? w / h : undefined;
}

const gcd = (a: number, b: number): number => (b ? gcd(b, a % b) : a);

/** The reduced "W:H" of a w×h size ("" when empty). */
export function aspectOf(w: number, h: number): AspectRatio {
  if (!(w > 0 && h > 0)) return "";
  const g = gcd(Math.round(w), Math.round(h));
  return `${Math.round(w) / g}:${Math.round(h) / g}`;
}

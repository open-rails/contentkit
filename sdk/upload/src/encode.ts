import type { EncodeProgress } from "./wire.gen.js";

/** Seconds left of an encode, counted down from when its report arrived (local clock; the server's `at` may be skewed). */
export function encodeRemaining(p: EncodeProgress | null | undefined, receivedAt: number, now: number): number | undefined {
  if (!p?.eta || p.stalled) return undefined;
  return Math.max(0, p.eta - (now - receivedAt) / 1000);
}

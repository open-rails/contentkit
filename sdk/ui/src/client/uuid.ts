/**
 * A random UUIDv7: Unix milliseconds, then 74 random bits. For ids the server
 * keeps as index keys, so new rows append. Secrets and session ids stay
 * crypto.randomUUID(): a UUIDv7 shows when it was made.
 */
export function uuidv7(now = Date.now()): string {
  const b = crypto.getRandomValues(new Uint8Array(16));
  for (let i = 5, ms = now; i >= 0; i--, ms = Math.floor(ms / 256)) b[i] = ms % 256;
  b[6] = 0x70 | (b[6]! & 0x0f);
  b[8] = 0x80 | (b[8]! & 0x3f);
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

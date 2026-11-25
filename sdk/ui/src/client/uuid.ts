/** n bytes from crypto.getRandomValues, which works outside secure contexts. Without crypto it throws: an id never degrades to a guessable one. */
function randomBytes(n: number): Uint8Array {
  if (typeof crypto === "undefined" || typeof crypto.getRandomValues !== "function") throw new Error("contentkit: crypto.getRandomValues is unavailable");
  return crypto.getRandomValues(new Uint8Array(n));
}

function format(b: Uint8Array): string {
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/**
 * A random UUIDv7: Unix milliseconds, then 74 random bits. For ids the server
 * keeps as index keys, so new rows append. Secrets and session ids use uuidv4:
 * a UUIDv7 shows when it was made.
 */
export function uuidv7(now = Date.now()): string {
  const b = randomBytes(16);
  for (let i = 5, ms = now; i >= 0; i--, ms = Math.floor(ms / 256)) b[i] = ms % 256;
  b[6] = 0x70 | (b[6]! & 0x0f);
  b[8] = 0x80 | (b[8]! & 0x3f);
  return format(b);
}

/** A random UUIDv4: 122 random bits and no instant, for bearer identities. */
export function uuidv4(): string {
  const b = randomBytes(16);
  b[6] = 0x40 | (b[6]! & 0x0f);
  b[8] = 0x80 | (b[8]! & 0x3f);
  return format(b);
}

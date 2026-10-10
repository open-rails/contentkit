// Records every sha256Hex call (aliased over src/hash.ts by the bench build).
import { sha256Hex as real } from "../src/hash.js";

export const hashLog: { start: number; end: number; bytes: number }[] = ((globalThis as any).__hashLog ??= []);

export async function sha256Hex(blob: Blob, opts?: Parameters<typeof real>[1]): Promise<string> {
  const start = performance.now();
  try {
    return await real(blob, opts);
  } finally {
    hashLog.push({ start, end: performance.now(), bytes: blob.size });
  }
}

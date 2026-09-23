import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { throwIfAborted } from "./errors.js";

const CHUNK = 4 << 20;

/**
 * Lowercase hex SHA-256 of blob, read in 4 MiB slices so memory stays bounded
 * (WebCrypto cannot digest incrementally).
 */
export async function sha256Hex(
  blob: Blob,
  opts: { signal?: AbortSignal; onProgress?: (hashed: number) => void } = {},
): Promise<string> {
  const h = sha256.create();
  for (let off = 0; off < blob.size; off += CHUNK) {
    throwIfAborted(opts.signal);
    h.update(new Uint8Array(await blob.slice(off, off + CHUNK).arrayBuffer()));
    opts.onProgress?.(Math.min(off + CHUNK, blob.size));
  }
  throwIfAborted(opts.signal);
  return bytesToHex(h.digest());
}

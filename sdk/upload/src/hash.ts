import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { throwIfAborted } from "./errors.js";
import { MAX_SINGLE_PUT } from "./wire.gen.js";

const CHUNK = 4 << 20;

/**
 * Lowercase hex SHA-256 of blob. Up to 64 MiB (every part and single PUT) it
 * is one WebCrypto digest: native and off the main thread, so parts hash in
 * parallel. Larger blobs, or no WebCrypto (an insecure context), stream
 * through @noble/hashes in 4 MiB slices so memory stays bounded.
 */
export async function sha256Hex(
  blob: Blob,
  opts: { signal?: AbortSignal; onProgress?: (hashed: number) => void } = {},
): Promise<string> {
  const subtle = globalThis.crypto?.subtle;
  if (subtle && blob.size <= MAX_SINGLE_PUT) {
    throwIfAborted(opts.signal);
    const digest = await subtle.digest("SHA-256", await blob.arrayBuffer());
    throwIfAborted(opts.signal);
    opts.onProgress?.(blob.size);
    return bytesToHex(new Uint8Array(digest));
  }
  const h = sha256.create();
  for (let off = 0; off < blob.size; off += CHUNK) {
    throwIfAborted(opts.signal);
    h.update(new Uint8Array(await blob.slice(off, off + CHUNK).arrayBuffer()));
    opts.onProgress?.(Math.min(off + CHUNK, blob.size));
  }
  throwIfAborted(opts.signal);
  return bytesToHex(h.digest());
}

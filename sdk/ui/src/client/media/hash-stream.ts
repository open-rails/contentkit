import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

const CHUNK = 4 << 20;

/**
 * Lowercase hex SHA-256 of blob, read in 4 MiB slices through @noble/hashes,
 * so memory stays bounded for any size. Shared by the hashing worker and
 * its in-thread fallback.
 */
export async function streamSha256(blob: Blob, onProgress?: (hashed: number) => void, aborted?: () => boolean): Promise<string> {
  const h = sha256.create();
  for (let off = 0; off < blob.size; off += CHUNK) {
    if (aborted?.()) throw new DOMException("aborted", "AbortError");
    h.update(new Uint8Array(await blob.slice(off, off + CHUNK).arrayBuffer()));
    onProgress?.(Math.min(off + CHUNK, blob.size));
  }
  return bytesToHex(h.digest());
}

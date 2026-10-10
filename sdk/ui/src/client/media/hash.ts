import { bytesToHex } from "@noble/hashes/utils.js";
import { aborted, throwIfAborted } from "./errors.js";
import { streamSha256 } from "./hash-stream.js";
import HashWorker from "./hash.worker.ts?worker&inline";
import { MAX_SINGLE_PUT } from "./wire.gen.js";

export interface HashOptions {
  signal?: AbortSignal;
  onProgress?: (hashed: number) => void;
}

/**
 * Lowercase hex SHA-256 of blob, off the main thread. Up to 64 MiB (every
 * part and single PUT) it is one WebCrypto digest: native, so parts hash in
 * parallel. Larger files stream through @noble/hashes in a Web Worker,
 * since WebCrypto cannot hash incrementally; without workers (or WebCrypto)
 * the same stream runs here.
 */
export async function sha256Hex(blob: Blob, o: HashOptions = {}): Promise<string> {
  throwIfAborted(o.signal);
  const subtle = globalThis.crypto?.subtle;
  if (subtle && blob.size <= MAX_SINGLE_PUT) {
    const digest = await subtle.digest("SHA-256", await blob.arrayBuffer());
    throwIfAborted(o.signal);
    o.onProgress?.(blob.size);
    return bytesToHex(new Uint8Array(digest));
  }
  const worked = typeof Worker === "function" ? await inWorker(blob, o) : undefined;
  if (worked !== undefined) return worked;
  try {
    return await streamSha256(blob, o.onProgress, () => !!o.signal?.aborted);
  } catch (err) {
    throw o.signal?.aborted ? aborted(o.signal) : err;
  }
}

/** The hash from a worker; undefined when no worker can run (a CSP without blob: or data: workers). */
function inWorker(blob: Blob, o: HashOptions): Promise<string | undefined> {
  return new Promise((resolve, reject) => {
    let w: Worker;
    try {
      w = new HashWorker();
    } catch {
      return resolve(undefined);
    }
    let started = false;
    const done = () => {
      w.terminate();
      o.signal?.removeEventListener("abort", stop);
    };
    const stop = () => {
      done();
      reject(aborted(o.signal));
    };
    o.signal?.addEventListener("abort", stop, { once: true });
    w.onerror = (e) => {
      e.preventDefault();
      done();
      if (started) reject(new Error("hashing failed: " + e.message));
      else resolve(undefined);
    };
    w.onmessage = (e: MessageEvent<{ hashed?: number; hex?: string; error?: string }>) => {
      started = true;
      const m = e.data;
      if (m.hashed !== undefined) return o.onProgress?.(m.hashed);
      done();
      if (m.hex) resolve(m.hex);
      else reject(new Error("hashing failed: " + m.error));
    };
    w.postMessage(blob);
  });
}

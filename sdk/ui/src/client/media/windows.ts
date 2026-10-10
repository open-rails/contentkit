import type { FileInfo, ReadResult } from "../generated/wire.js";

/** Files per window when a read is split into windows. */
export const READ_CHUNK = 50;

/** Offsets of chunk-sized windows covering files [start, end), clipped to total when known. */
export function chunkOffsets(start: number, end: number, total?: number, chunk = READ_CHUNK): number[] {
  const out: number[] = [];
  const stop = total === undefined ? end : Math.min(end, total);
  if (start >= stop) return out;
  for (let o = Math.floor(Math.max(start, 0) / chunk) * chunk; o < stop; o += chunk) out.push(o);
  return out;
}

/**
 * One item's window reads as one read: the newest window's listing and
 * metadata, each file's URL from the newest window that signed it, and the
 * earliest expiry. Every window lists every file; only its range has URLs.
 */
export function mergeReads(reads: readonly (ReadResult | null | undefined)[]): ReadResult | null {
  const present = reads.filter((r): r is ReadResult => !!r);
  if (present.length === 0) return null;
  if (present.length === 1) return present[0]!;
  const newest = present.reduce((a, b) => (b.expires >= a.expires ? b : a));
  const urls = new Map<string, FileInfo>();
  for (const r of [...present].sort((a, b) => a.expires - b.expires)) for (const f of r.files) if (f.url) urls.set(f.path, f);
  const files = newest.files.map((f) => {
    if (f.url || f.locked) return f;
    const signed = urls.get(f.path);
    return signed ? { ...f, url: signed.url, ...(signed.download ? { download: signed.download } : {}) } : f;
  });
  const start = Math.min(...present.map((r) => r.offset));
  const end = Math.max(...present.map((r) => r.offset + r.limit));
  return { ...newest, files, offset: start, limit: end - start, expires: Math.min(...present.map((r) => r.expires)) };
}

/** An upload the worker has not finished: staged, or with work pending, and not failed. */
export const isProcessing = (f: FileInfo) => !!f.upload && !f.failed && (!!f.staged || (f.pending?.length ?? 0) > 0);

/** An editor read with uploads still processing (not stalled because the item is full). */
export function processing(read: ReadResult | null | undefined): boolean {
  if (!read || read.full) return false;
  return read.state === "processing" || read.files.some(isProcessing);
}

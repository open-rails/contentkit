import { UploadApi, type ApiOptions } from "./api.js";
import { UploadError, aborted, throwIfAborted } from "./errors.js";
import { sha256Hex } from "./hash.js";
import { Pacer } from "./pacer.js";
import { defaultTransport, type Transport } from "./transport.js";
import { MAX_SINGLE_PUT, type CommitFile, type Op, type RefBody, type RequestReply } from "./wire.gen.js";

export interface ClientOptions extends ApiOptions {
  transport?: Transport;
  /** Parallel parts per multipart upload (the pacer may run fewer). Default 4. */
  concurrency?: number;
  /** Retries per part and per idempotent API call. Default 5. */
  retries?: number;
  /** Delay before retry attempt (0-based); retryAfter is the server's hint in seconds. */
  retryDelay?: (attempt: number, retryAfter?: number) => number;
  /** Seconds one part should take. Default 30. */
  targetPartSeconds?: number;
}

export interface Progress {
  phase: "hashing" | "uploading" | "completing";
  loaded: number;
  total: number;
}

export interface UploadOptions {
  ref: RefBody;
  /** Content type; default the file's. */
  type?: string;
  /** Upload the kind's slot original instead of a manifest file. */
  slot?: string;
  /** Aborting pauses: the multipart upload stays resumable from the last state. */
  signal?: AbortSignal;
  onProgress?: (p: Progress) => void;
  /** State saved from onState, to resume a multipart upload of the same file. */
  resume?: UploadState;
  /** Resumable state after every change; null once the upload completed. Persist it to survive a reload. */
  onState?: (s: UploadState | null) => void;
}

/** An uploaded original, ready for an insert or replace op. */
export interface UploadedFile {
  name: string;
  type: string;
  size: number;
  /** Set for single-PUT uploads (and slots). */
  sha256?: string;
  /** The identical file was already in the item's folder; nothing was sent. */
  exists: boolean;
}

export interface PlannedPart {
  number: number;
  offset: number;
  size: number;
  sha256?: string;
}

/** A multipart upload in flight; JSON-serializable. */
export interface UploadState {
  ticket: string;
  name: string;
  type: string;
  size: number;
  ref: RefBody;
  file: { name?: string; lastModified?: number; size: number };
  limits: { minPartSize: number; maxPartSize: number; maxParts: number };
  parts: PlannedPart[];
}

/** A file to upload again when its original is gone at commit; type defaults to the file's. */
export type CommitSource = Blob | { file: Blob; type?: string };

export interface CommitOptions {
  signal?: AbortSignal;
  /** Original name → the file it was uploaded from. */
  sources?: Record<string, CommitSource>;
}

type Uploadable = Blob & { name?: string; lastModified?: number };

export class UploadClient {
  readonly api: UploadApi;
  private readonly transport: Transport;
  private readonly concurrency: number;
  private readonly retries: number;
  private readonly delay: (attempt: number, retryAfter?: number) => number;
  private readonly targetSeconds: number;

  constructor(o: ClientOptions) {
    this.api = new UploadApi(o);
    this.transport = o.transport ?? defaultTransport;
    this.concurrency = o.concurrency ?? 4;
    this.retries = o.retries ?? 5;
    this.delay = o.retryDelay ?? backoff;
    this.targetSeconds = o.targetPartSeconds ?? 30;
  }

  /**
   * Uploads file into the item's folder: one checksum-bound PUT up to 64 MiB
   * (or for a slot), resumable multipart above. A refused presign throws
   * before any bytes move; multipart presigns before hashing anything.
   */
  async upload(file: Uploadable, o: UploadOptions): Promise<UploadedFile> {
    const type = o.type || file.type;
    if (!type) throw new UploadError("invalid_request", "the file has no content type");
    throwIfAborted(o.signal);
    if (o.resume) return this.resumeMultipart(file, o.resume, o);
    if (o.slot || file.size <= MAX_SINGLE_PUT) return this.single(file, type, o);

    const p = await this.api.presign({ ref: o.ref, type, size: file.size }, o.signal);
    if (!p.multipart) throw new UploadError("invalid_request", "expected a multipart upload plan");
    const m = p.multipart;
    const state: UploadState = {
      ticket: m.ticket,
      name: p.name,
      type,
      size: file.size,
      ref: o.ref,
      file: fingerprint(file),
      limits: { minPartSize: m.min_part_size, maxPartSize: m.max_part_size, maxParts: m.max_parts },
      parts: [],
    };
    return this.multipart(file, state, new Set(), o);
  }

  /** Uploads a slot original and commits it (re-encodes the slot's outputs). */
  async uploadSlot(file: Uploadable, o: UploadOptions & { slot: string }): Promise<UploadedFile> {
    const f = await this.upload(file, o);
    await this.retry(() => this.api.commitSlot({ ref: o.ref, slot: o.slot, sha256: f.sha256! }, o.signal), o.signal);
    return f;
  }

  /**
   * Applies manifest ops in one conditional write; returns the committed file
   * order. With sources (original name → the file uploaded as it), a
   * not_uploaded refusal (the original expired or is due for cleanup) uploads
   * the affected files again and retries the commit once.
   */
  async commit(ref: RefBody, ops: Op[], o: CommitOptions = {}): Promise<CommitFile[]> {
    try {
      return (await this.api.commit({ ref, ops }, o.signal)).files;
    } catch (e) {
      const sources = o.sources ?? {};
      if (!(e instanceof UploadError) || e.code !== "not_uploaded") throw e;
      const originals = [...new Set(ops.map((op) => op.original).filter((n): n is string => !!n && n in sources))];
      const stale = e.originals ? originals.filter((n) => e.originals!.includes(n)) : originals;
      if (stale.length === 0) throw e;
      const renamed = new Map<string, string>();
      for (const name of stale) {
        const src = sources[name]!;
        const file = src instanceof Blob ? src : src.file;
        const type = src instanceof Blob ? undefined : src.type;
        const up = await this.upload(file, { ref, type, signal: o.signal });
        renamed.set(name, up.name);
      }
      const retried = ops.map((op) => (op.original && renamed.has(op.original) ? { ...op, original: renamed.get(op.original) } : op));
      return (await this.api.commit({ ref, ops: retried }, o.signal)).files;
    }
  }

  /** Discards a paused multipart upload. */
  async discard(state: UploadState): Promise<void> {
    await this.api.abort({ ticket: state.ticket });
  }

  private async single(file: Uploadable, type: string, o: UploadOptions): Promise<UploadedFile> {
    const total = file.size;
    const sha256 = await sha256Hex(file, {
      signal: o.signal,
      onProgress: (loaded) => o.onProgress?.({ phase: "hashing", loaded, total }),
    });
    const presign = () => this.api.presign({ ref: o.ref, type, size: total, sha256, slot: o.slot }, o.signal);
    const p = await presign();
    const out: UploadedFile = { name: p.name, type, size: total, sha256, exists: !!p.exists };
    if (p.exists) return out;
    let put = p.put!;
    await this.retry(
      async () => {
        await this.transport(put, file, {
          signal: o.signal,
          onProgress: (loaded) => o.onProgress?.({ phase: "uploading", loaded, total }),
        });
      },
      o.signal,
      async (err) => {
        if (err.code === "storage" && err.status === 403) put = (await presign()).put!; // expired URL
      },
    );
    return out;
  }

  private async resumeMultipart(file: Uploadable, state: UploadState, o: UploadOptions): Promise<UploadedFile> {
    const fp = fingerprint(file);
    if (fp.size !== state.size || fp.name !== state.file.name || fp.lastModified !== state.file.lastModified) {
      throw new UploadError("resume_mismatch", "the saved upload is for a different file");
    }
    let listed;
    try {
      listed = await this.retry(() => this.api.listParts({ ticket: state.ticket }, o.signal), o.signal);
    } catch (err) {
      // Gone: already completed (Complete is idempotent) or expired.
      if (err instanceof UploadError && err.code === "not_found") return this.complete(state, o);
      throw err;
    }
    const landed = new Set<number>();
    for (const l of listed.parts) {
      const plan = state.parts.find((p) => p.number === l.number);
      if (!plan || plan.size !== l.size || (l.sha256 && plan.sha256 !== l.sha256)) {
        // Parts this state does not describe: the upload cannot be finished consistently.
        await this.discard(state).catch(() => {});
        o.onState?.(null);
        return this.upload(file, { ...o, resume: undefined });
      }
      landed.add(l.number);
    }
    return this.multipart(file, structuredClone(state), landed, o);
  }

  private async multipart(file: Uploadable, state: UploadState, landed: Set<number>, o: UploadOptions): Promise<UploadedFile> {
    const pacer = new Pacer({ ...state.limits, maxConcurrency: this.concurrency, targetSeconds: this.targetSeconds });
    const emitState = () => o.onState?.(structuredClone(state));
    const sending = new Map<number, number>();
    let sent = state.parts.filter((p) => landed.has(p.number)).reduce((n, p) => n + p.size, 0);
    const emitProgress = () => {
      let loaded = sent;
      for (const n of sending.values()) loaded += n;
      o.onProgress?.({ phase: "uploading", loaded, total: state.size });
    };
    const pending = state.parts.filter((p) => !landed.has(p.number));
    let offset = state.parts.reduce((end, p) => Math.max(end, p.offset + p.size), 0);
    const next = (): PlannedPart | undefined => {
      const p = pending.shift();
      if (p || offset >= state.size) return p;
      const part: PlannedPart = {
        number: state.parts.length + 1,
        offset,
        size: Math.min(pacer.partSize(), state.size - offset),
      };
      offset += part.size;
      state.parts.push(part);
      return part;
    };

    const stop = new AbortController();
    const signal = o.signal ? AbortSignal.any([o.signal, stop.signal]) : stop.signal;
    const run = async (part: PlannedPart) => {
      const body = file.slice(part.offset, part.offset + part.size);
      await this.retry(async () => {
        if (!part.sha256) {
          part.sha256 = await sha256Hex(body, { signal });
          emitState();
        }
        const { parts } = await this.api.parts(
          { ticket: state.ticket, parts: [{ number: part.number, size: part.size, sha256: part.sha256 }] },
          signal,
        );
        const req = parts[0]?.request as RequestReply;
        const began = performance.now();
        const alongside = inFlight.size;
        try {
          await this.transport(req, body, {
            signal,
            onProgress: (n) => {
              sending.set(part.number, n);
              emitProgress();
            },
          });
        } finally {
          sending.delete(part.number);
        }
        pacer.record(part.size, performance.now() - began, alongside);
        sent += part.size;
        emitProgress();
      }, signal);
    };

    emitState();
    emitProgress();
    const inFlight = new Set<Promise<void>>();
    let failure: unknown;
    for (;;) {
      let part: PlannedPart | undefined;
      while (failure === undefined && inFlight.size < pacer.concurrency && (part = next())) {
        emitState();
        const job: Promise<void> = run(part).then(
          () => void inFlight.delete(job),
          (err) => {
            inFlight.delete(job);
            if (failure === undefined) {
              failure = err;
              stop.abort();
            }
          },
        );
        inFlight.add(job);
      }
      if (inFlight.size === 0) break;
      await Promise.race(inFlight);
    }
    if (failure !== undefined) {
      throw o.signal?.aborted ? aborted(o.signal) : failure;
    }
    return this.complete(state, o);
  }

  private async complete(state: UploadState, o: UploadOptions): Promise<UploadedFile> {
    o.onProgress?.({ phase: "completing", loaded: state.size, total: state.size });
    const c = await this.retry(() => this.api.complete({ ticket: state.ticket }, o.signal), o.signal);
    o.onState?.(null);
    return { name: c.name, type: c.type, size: c.size, exists: false };
  }

  private async retry<T>(fn: () => Promise<T>, signal?: AbortSignal, before?: (err: UploadError) => Promise<void>): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      try {
        return await fn();
      } catch (e) {
        const err = e instanceof UploadError ? e : new UploadError("network", String(e), 0, undefined, { cause: e });
        if (signal?.aborted) throw aborted(signal);
        if (!err.transient || attempt >= this.retries) throw err;
        await sleep(this.delay(attempt, err.retryAfter), signal);
        await before?.(err);
      }
    }
  }
}

export function createUploadClient(o: ClientOptions): UploadClient {
  return new UploadClient(o);
}

function fingerprint(f: Uploadable): UploadState["file"] {
  return { name: f.name, lastModified: f.lastModified, size: f.size };
}

/** Exponential backoff from 1 s to 30 s with jitter, or the server's Retry-After. */
export function backoff(attempt: number, retryAfter?: number): number {
  if (retryAfter) return retryAfter * 1000;
  const base = Math.min(30_000, 1000 * 2 ** attempt);
  return base * (0.75 + Math.random() * 0.5);
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(aborted(signal));
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(aborted(signal));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

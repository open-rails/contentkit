import { UploadApi, type ApiOptions, type ReadOptions } from "./api.js";
import { UploadError, aborted, failureError, throwIfAborted } from "./errors.js";
import { sha256Hex } from "./hash.js";
import { Pacer } from "./pacer.js";
import { defaultTransport, type Transport } from "./transport.js";
import type { Edit, FileInfo, Op, PresignReply, ReadResult, RefBody, RequestReply } from "./wire.gen.js";

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
  /** processing: committed, waiting for the worker (put). */
  phase: "hashing" | "uploading" | "completing" | "processing";
  loaded: number;
  total: number;
}

export interface UploadOptions {
  ref: RefBody;
  /** The upload path: a literal ("cover") or under a pattern ("originals/001.png"); the server cleans it and adds the extension. */
  path: string;
  /** Content type; default the file's. */
  type?: string;
  /** Aborting pauses: a multipart upload stays resumable from the last state. */
  signal?: AbortSignal;
  onProgress?: (p: Progress) => void;
  /** State saved from onState, to resume a multipart upload of the same file. */
  resume?: UploadState;
  /** Resumable state after every change; null once the upload completed. Persist it to survive a reload. */
  onState?: (s: UploadState | null) => void;
}

/** An upload, ready for a put op. */
export interface UploadedFile {
  /** The path to commit: cleaned, with an extension, named by the server for a Named upload. */
  path: string;
  /**
   * The name to commit: the folder's identical blob ("sha256-{hex}") when
   * exists, else the staged upload ("u-{uuid}") the worker hashes and places
   * after the commit.
   */
  blob: string;
  type: string;
  size: number;
  /** The identical file was already in the item's folder; nothing was sent. */
  exists: boolean;
  /** The host processes files on upload: commit it unattached now (the queue does). */
  processOnUpload?: boolean;
}

/** The put op's fields besides path and blob. */
export interface PutOptions {
  /** Refuse to replace an existing upload. Default false. */
  createOnly?: boolean;
  edit?: Edit | null;
  meta?: Record<string, unknown>;
  index?: number;
  /** Wait until the worker processed the upload (waitFor). Default true. */
  wait?: boolean;
  /** How long to wait, ms. */
  timeout?: number;
  /** The upload, before the commit. */
  onUploaded?: (up: UploadedFile) => void;
}

export interface WaitOptions {
  signal?: AbortSignal;
  /** Poll interval, ms. Default 1000. */
  interval?: number;
  /** ms; then render_timeout. Default 120000. */
  timeout?: number;
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
  path: string;
  /** The staged upload ("u-{uuid}") the parts write. */
  blob: string;
  type: string;
  size: number;
  ref: RefBody;
  file: { name?: string; lastModified?: number; size: number };
  limits: { minPartSize: number; maxPartSize: number; maxParts: number };
  parts: PlannedPart[];
  processOnUpload?: boolean;
}

/** A file to upload again when its blob is gone at commit; type defaults to the file's. */
export type CommitSource = Blob | { file: Blob; type?: string };

export interface CommitOptions {
  signal?: AbortSignal;
  /** Blob ("sha256-…") → the file it was uploaded from. */
  sources?: Record<string, CommitSource>;
}

type Uploadable = Blob & { name?: string; lastModified?: number };

/** A path names a file by its full path or, for an upload, its stem ("cover" for "cover.png"). */
export const samePath = (file: string, path: string) => file === path || stem(file) === path;

/** The path without its extension. */
export const stem = (path: string) => {
  const i = path.lastIndexOf(".");
  return i > path.lastIndexOf("/") + 1 ? path.slice(0, i) : path;
};

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
   * Uploads file to the item's folder: it hashes the whole file (off the
   * main thread) and presigns {path, type, size, sha256}, then sends nothing
   * (the folder holds the identical blob), or stages it: one checksum-bound
   * PUT up to 64 MiB, resumable multipart above. A refused presign throws
   * before any bytes move. Commit the result with a put op (or use put());
   * the worker then places a staged upload at the hash of its bytes.
   */
  async upload(file: Uploadable, o: UploadOptions): Promise<UploadedFile> {
    const type = o.type || file.type;
    if (!type) throw new UploadError("invalid_request", "the file has no content type");
    throwIfAborted(o.signal);
    if (o.resume) return this.resumeMultipart(file, o.resume, o);
    const total = file.size;
    const sha256 = await sha256Hex(file, { signal: o.signal, onProgress: (loaded) => o.onProgress?.({ phase: "hashing", loaded, total }) });
    const presign = () => this.api.presign({ ref: o.ref, path: o.path, type, size: total, sha256 }, o.signal);
    let p = await presign();
    const out = (p: PresignReply): UploadedFile => ({ path: p.path, blob: p.blob, type, size: total, exists: !!p.exists, processOnUpload: !!p.process_on_upload });
    if (p.exists) return out(p);
    if (p.multipart) {
      const m = p.multipart;
      const state: UploadState = {
        ticket: m.ticket,
        path: p.path,
        blob: p.blob,
        type,
        size: total,
        ref: o.ref,
        file: fingerprint(file),
        limits: { minPartSize: m.min_part_size, maxPartSize: m.max_part_size, maxParts: m.max_parts },
        parts: [],
        processOnUpload: !!p.process_on_upload,
      };
      return this.multipart(file, state, new Set(), o);
    }
    await this.retry(
      async () => {
        if (p.exists) return;
        await this.transport(p.put!, file, { signal: o.signal, onProgress: (loaded) => o.onProgress?.({ phase: "uploading", loaded, total }) });
      },
      o.signal,
      async (err) => {
        // An expired URL: presign again (a Named upload gets a new name).
        if (err.code === "storage" && err.status === 403) p = await presign();
      },
    );
    return out(p);
  }

  /**
   * Uploads file and commits it as a put to its path (with edit, meta and
   * index), then by default waits until the worker processed it; resolves
   * with the upload as an editor reads it.
   */
  async put(file: Uploadable, o: UploadOptions & PutOptions): Promise<FileInfo> {
    const up = await this.upload(file, o);
    o.onUploaded?.(up);
    const op: Op = { op: "put", path: up.path, blob: up.blob };
    if (o.createOnly) op.create_id = crypto.randomUUID();
    if (o.edit) op.edit = o.edit;
    if (o.meta) op.meta = o.meta;
    if (o.index !== undefined) op.index = o.index;
    const files = await this.commit(o.ref, [op], { signal: o.signal, sources: { [up.blob]: { file, type: up.type } } });
    if (o.wait === false) return files.find((f) => f.path === up.path) ?? { path: up.path, type: up.type };
    o.onProgress?.({ phase: "processing", loaded: up.size, total: up.size });
    return this.waitFor(o.ref, up.path, { signal: o.signal, timeout: o.timeout });
  }

  /**
   * Applies ops to the item in one conditional write; returns its uploads as
   * an editor reads them. With sources (blob → the file uploaded as it), a
   * not_uploaded refusal (the blob expired or is due for cleanup) uploads
   * the affected files again and retries the commit once.
   */
  async commit(ref: RefBody, ops: Op[], o: CommitOptions = {}): Promise<FileInfo[]> {
    try {
      return (await this.api.commit({ ref, ops }, o.signal)).files;
    } catch (e) {
      if (!(e instanceof UploadError) || e.code !== "not_uploaded") throw e;
      const sources = o.sources ?? {};
      const puts = ops.filter((op) => op.op === "put" && op.blob && op.blob in sources);
      const stale = [...new Set(puts.map((op) => op.blob!))].filter((b) => !e.blobs || e.blobs.includes(b));
      if (stale.length === 0) throw e;
      const renamed = new Map<string, string>();
      for (const blob of stale) {
        const src = sources[blob]!;
        const file = src instanceof Blob ? src : src.file;
        const type = src instanceof Blob ? undefined : src.type;
        const op = puts.find((p) => p.blob === blob)!;
        renamed.set(blob, (await this.upload(file, { ref, path: op.path!, type, signal: o.signal })).blob);
      }
      const retried = ops.map((op) => (op.blob && renamed.has(op.blob) ? { ...op, blob: renamed.get(op.blob) } : op));
      return (await this.api.commit({ ref, ops: retried }, o.signal)).files;
    }
  }

  /** The read API: the item's files under prefix in manifest order, with signed URLs for what this viewer may have. */
  async read(ref: RefBody, o: ReadOptions & { signal?: AbortSignal } = {}): Promise<ReadResult> {
    this.api.itemURL(ref); // a bad ref or a missing readEndpoint fails at once
    return this.retry(() => this.api.read(ref, o, o.signal), o.signal);
  }

  /**
   * Polls an editor read until the upload at path (or with that stem) is
   * processed: placed (not staged), nothing pending and its blob present (a
   * frame grabbed).
   * Rejects with its failure, not_found once it is gone, too_large while the
   * item is full (nothing is processed until uploads are removed),
   * render_timeout after the timeout.
   */
  async waitFor(ref: RefBody, path: string, o: WaitOptions = {}): Promise<FileInfo> {
    const until = Date.now() + (o.timeout ?? 120_000);
    for (;;) {
      const r = await this.read(ref, { editor: true, prefix: stem(path), signal: o.signal });
      const f = r.files.find((x) => x.upload && samePath(x.path, path));
      if (!f) throw new UploadError("not_found", `no upload ${path}`, 404);
      if (f.failed) throw failureError(f.failed);
      if (!f.pending?.length && !f.staged && (f.size ?? 0) > 0) return f;
      if (r.full) throw new UploadError("too_large", `${path} waits: the item is full; remove uploads to process more`, 413);
      if (Date.now() >= until) throw new UploadError("render_timeout", `${path} is still processing`);
      await sleep(o.interval ?? 1000, o.signal);
    }
  }

  /**
   * The upload's editor view, for re-cropping: its URL and the upload's
   * oriented size. Waits while it renders (an editor read asks for it);
   * not_found when the path has no upload, render_timeout after the timeout
   * (default 30 s).
   */
  async editorView(ref: RefBody, path: string, o: WaitOptions = {}): Promise<{ url: string; width: number; height: number }> {
    const until = Date.now() + (o.timeout ?? 30_000);
    for (;;) {
      const r = await this.read(ref, { editor: true, prefix: stem(path), signal: o.signal });
      const f = r.files.find((x) => x.upload && samePath(x.path, path));
      if (!f) throw new UploadError("not_found", `no upload ${path}`, 404);
      if (f.editor_url && f.w && f.h) return { url: f.editor_url, width: f.w, height: f.h };
      if (f.failed) throw failureError(f.failed);
      if (Date.now() >= until) throw new UploadError("render_timeout", `the editor view of ${path} is still rendering`);
      await sleep(o.interval ?? 1000, o.signal);
    }
  }

  /** A JPEG still of the video upload at path, t seconds in, w pixels wide (default the frame's). */
  getFrame(ref: RefBody, path: string, t: number, w?: number, signal?: AbortSignal): Promise<Blob> {
    return this.retry(() => this.api.frame(ref, path, t, w, signal), signal);
  }

  /** The folder of an HLS ladder from a read's `hls`: `{readEndpoint}/{kind}/{id}/hls/{dir}`; master.m3u8 and sprite.vtt resolve under it. */
  hlsBase(ref: RefBody, dir: string): string {
    return `${this.api.itemURL(ref)}/hls/${dir}`;
  }

  /** Discards a paused multipart upload. */
  async discard(state: UploadState): Promise<void> {
    await this.api.abort({ ticket: state.ticket });
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
    // Parts hash and presign ahead of the PUT slots, so a freed slot starts
    // its next PUT at once; pacer.concurrency bounds only the PUTs.
    let putting = 0;
    const waiting: (() => void)[] = [];
    const acquire = () =>
      new Promise<void>((resolve, reject) => {
        const take = () => {
          signal.removeEventListener("abort", drop);
          putting++;
          resolve();
        };
        const drop = () => {
          waiting.splice(waiting.indexOf(take) >>> 0, 1);
          reject(aborted(signal));
        };
        if (putting < pacer.concurrency) return take();
        if (signal.aborted) return reject(aborted(signal));
        signal.addEventListener("abort", drop, { once: true });
        waiting.push(take);
      });
    const release = () => {
      putting--;
      while (putting < pacer.concurrency && waiting.length) waiting.shift()!();
    };
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
        await acquire();
        const began = performance.now();
        const alongside = putting;
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
          release();
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
      while (failure === undefined && inFlight.size < pacer.concurrency + 2 && (part = next())) {
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
    return { path: state.path, blob: c.blob, type: c.type, size: c.size, exists: false, processOnUpload: state.processOnUpload };
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

export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
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

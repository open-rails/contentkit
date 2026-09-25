import { UploadApi, type ApiOptions } from "./api.js";
import { UploadError, aborted, throwIfAborted, type UploadErrorCode } from "./errors.js";
import { sha256Hex } from "./hash.js";
import { Pacer } from "./pacer.js";
import { defaultTransport, type Transport } from "./transport.js";
import { MAX_SINGLE_PUT, type CommitFile, type Edit, type FileInfo, type Op, type RefBody, type RequestReply, type SlotManifest, type VideoImages } from "./wire.gen.js";

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
  /** Upload a new inline image (post bodies, poll options); the result's name is its id. */
  inline?: boolean;
  /** Aborting pauses: the multipart upload stays resumable from the last state. */
  signal?: AbortSignal;
  onProgress?: (p: Progress) => void;
  /** State saved from onState, to resume a multipart upload of the same file. */
  resume?: UploadState;
  /** Resumable state after every change; null once the upload completed. Persist it to survive a reload. */
  onState?: (s: UploadState | null) => void;
}

export interface SlotUploadOptions extends UploadOptions {
  slot: string;
  /** Crop (EXIF-oriented original pixels) and rotation; omitted = centred at the slot's aspect. */
  edit?: Edit | null;
}

/** A committed slot original and the slot as rendered from it. */
export interface SlotUpload extends UploadedFile {
  manifest: SlotManifest;
}

/** An uploaded original, ready for an insert or replace op. */
export interface UploadedFile {
  name: string;
  type: string;
  size: number;
  /** Set for single-PUT uploads (and slots and inline images). */
  sha256?: string;
  /** The identical file was already in the item's folder; nothing was sent. */
  exists: boolean;
  /** The host processes files on upload: commit it unattached now (the queue does). */
  processOnUpload?: boolean;
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
  processOnUpload?: boolean;
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
    if (o.slot || o.inline || file.size <= MAX_SINGLE_PUT) return this.single(file, type, o);

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
      processOnUpload: !!p.process_on_upload,
    };
    return this.multipart(file, state, new Set(), o);
  }

  /** Uploads a slot original and commits it with the edit; the server renders every size. */
  async uploadSlot(file: Uploadable, o: SlotUploadOptions): Promise<SlotUpload> {
    const f = await this.upload(file, o);
    const body = { ref: o.ref, slot: o.slot, sha256: f.sha256!, ...(o.edit ? { edit: o.edit } : {}), ...fileName(file) };
    const manifest = await this.retry(() => this.api.commitSlot(body, o.signal), o.signal);
    return { ...f, manifest };
  }

  /** Re-renders a slot from its committed original with a new edit (null: centred); nothing is uploaded. */
  editSlot(ref: RefBody, slot: string, edit?: Edit | null, signal?: AbortSignal): Promise<SlotManifest> {
    return this.retry(() => this.api.editSlot({ ref, slot, ...(edit ? { edit } : {}) }, signal), signal);
  }

  /** The slot's aspect, edit, dims and rendered sizes (no outputs before the first commit). */
  getSlot(ref: RefBody, slot: string, signal?: AbortSignal): Promise<SlotManifest> {
    return this.retry(() => this.api.slot({ ref, slot }, signal), signal);
  }

  /** The committed original, for re-cropping; not_found when the slot has none. */
  getSlotOriginal(ref: RefBody, slot: string, signal?: AbortSignal): Promise<Blob> {
    return this.retry(() => this.api.slotOriginal({ ref, slot }, signal), signal);
  }

  /**
   * Polls until the slot's outputs are encoded (pending false) or timeout ms
   * pass; returns the latest manifest either way.
   */
  async waitForSlot(ref: RefBody, slot: string, o: { signal?: AbortSignal; interval?: number; timeout?: number } = {}): Promise<SlotManifest> {
    const until = Date.now() + (o.timeout ?? 60_000);
    for (;;) {
      const m = await this.getSlot(ref, slot, o.signal);
      if (!m.pending || Date.now() >= until) return m;
      await sleep(o.interval ?? 1000, o.signal);
    }
  }

  /**
   * Uploads a new inline image, commits it and waits until it is rendered;
   * url is its public URL. Hand name to the host (a post body, a cover).
   */
  async uploadInline(file: Uploadable, o: Omit<UploadOptions, "slot" | "inline" | "resume">): Promise<UploadedFile & { url: string }> {
    const f = await this.upload(file, { ...o, inline: true });
    let m = await this.retry(() => this.api.commitSlot({ ref: o.ref, slot: f.name, sha256: f.sha256!, ...fileName(file) }, o.signal), o.signal);
    if (m.pending) m = await this.waitForSlot(o.ref, f.name, { signal: o.signal, interval: 500 });
    const url = m.outputs.at(-1)?.url;
    if (!url) throw new UploadError((m.error_code ?? "internal_error") as UploadErrorCode, m.error ?? "the image was not rendered", 422);
    return { ...f, url };
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

  /**
   * Sets a file's non-destructive edit (crop in original pixels, then a
   * clockwise rotate); null clears it. Its variants are re-derived from the
   * untouched original.
   */
  async edit(ref: RefBody, name: string, edit: Edit | null, o: { signal?: AbortSignal } = {}): Promise<CommitFile[]> {
    return this.commit(ref, [{ op: "edit", name, ...(edit ? { edit } : {}) }], o);
  }

  /**
   * Makes a manifest image the slot's original (a cover from a page), through
   * edit: omitted uses the file's own edit, {} none. With a slot aspect the
   * server derives the crop's height from its width. o.from names another
   * item holding file (e.g. a channel avatar from a post image).
   */
  async setSlotFromFile(ref: RefBody, slot: string, file: string, edit?: Edit, o: { signal?: AbortSignal; from?: RefBody } = {}): Promise<SlotManifest> {
    const body = { ref, slot, file, ...(edit ? { edit } : {}), ...(o.from ? { from: o.from } : {}) };
    return this.retry(() => this.api.commitSlotFromFile(body, o.signal), o.signal);
  }

  /** A video item's poster, with its selection and the video's duration and frame size. */
  getVideoImages(ref: RefBody, file?: string, signal?: AbortSignal): Promise<VideoImages> {
    return this.retry(() => this.api.videoImages({ ref, ...(file ? { file } : {}) }, signal), signal);
  }

  /**
   * Sets the poster to a frame (seconds into file; edit in the frame's pixels,
   * VideoInfo w×h) or back to the automatic frame; the worker grabs it and the
   * image job renders the sizes.
   */
  setVideoPoster(ref: RefBody, poster: { source: "frame"; time: number; file?: string; edit?: Edit | null } | { source: "auto"; file?: string }, signal?: AbortSignal): Promise<VideoImages> {
    const edit = "edit" in poster && poster.edit ? { edit: poster.edit } : {};
    const time = poster.source === "frame" ? { time: poster.time } : {};
    const body = { ref, source: poster.source, ...(poster.file ? { file: poster.file } : {}), ...time, ...edit };
    return this.retry(() => this.api.videoPoster(body, signal), signal);
  }

  /** Uploads an image as the poster, cropped by edit (its own pixels; omitted: the whole image). */
  async uploadVideoPoster(image: Uploadable, o: { ref: RefBody; edit?: Edit | null; signal?: AbortSignal; onProgress?: (p: Progress) => void }): Promise<VideoImages> {
    const f = await this.upload(image, { ref: o.ref, slot: "poster", signal: o.signal, onProgress: o.onProgress });
    const body = { ref: o.ref, source: "upload" as const, sha256: f.sha256!, ...(o.edit ? { edit: o.edit } : {}) };
    return this.retry(() => this.api.videoPoster(body, o.signal), o.signal);
  }

  /** One frame as a JPEG w pixels wide (clamped by the server to 64–1280 and the video). */
  getFrame(ref: RefBody, time: number, o: { file?: string; width?: number; signal?: AbortSignal } = {}): Promise<Blob> {
    return this.retry(() => this.api.frame({ ...ref, file: o.file, t: time, w: o.width }, o.signal), o.signal);
  }

  /** Polls until the poster is rendered, or timeout ms pass; returns the latest either way. */
  async waitForVideoImages(ref: RefBody, o: { file?: string; signal?: AbortSignal; interval?: number; timeout?: number } = {}): Promise<VideoImages> {
    const until = Date.now() + (o.timeout ?? 120_000);
    for (;;) {
      const v = await this.getVideoImages(ref, o.file, o.signal);
      if (!v.poster.pending || Date.now() >= until) return v;
      await sleep(o.interval ?? 1500, o.signal);
    }
  }

  /** The item's named files as their editor reads them, unattached ones included (processing state, progress). */
  files(ref: RefBody, names?: string[], signal?: AbortSignal): Promise<FileInfo[]> {
    return this.retry(async () => (await this.api.files({ ref, ...(names?.length ? { names } : {}) }, signal)).files, signal);
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
    const presign = () => this.api.presign({ ref: o.ref, type, size: total, sha256, slot: o.slot, inline: o.inline }, o.signal);
    const p = await presign();
    const out: UploadedFile = { name: p.name, type, size: total, sha256, exists: !!p.exists, processOnUpload: !!p.process_on_upload };
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
        if (err.code === "storage" && err.status === 403) {
          const again = await presign(); // expired URL; an inline image gets a new name
          put = again.put!;
          out.name = again.name;
        }
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
    return { name: c.name, type: c.type, size: c.size, exists: false, processOnUpload: state.processOnUpload };
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

/** The uploaded file's name for commit-slot, when it has one. */
function fileName(file: Uploadable): { filename?: string } {
  return file.name ? { filename: file.name } : {};
}

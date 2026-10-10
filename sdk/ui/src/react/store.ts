import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { FileInfo, ReadResult, RefBody } from "../client/generated/wire.js";
import type { ContentKitChange } from "../client/http.js";
import type { ReadOptions } from "../client/media/api.js";
import { samePath, sleep } from "../client/media/client.js";
import { isProcessing, processing } from "../client/media/windows.js";
import { expiryDelay } from "../client/playback.js";

export interface ReadEntry {
  read: ReadResult | null;
  loading: boolean;
  /** A read or set() has completed. */
  loaded: boolean;
  error?: ContentKitError;
}

/** What one reader asks of a shared read beyond the read itself. */
export interface ReadPolicy {
  /** Poll at this interval (ms) while uploads process (editor reads). */
  poll?: number | false;
  /** Read again shortly before the URLs expire. */
  refresh?: boolean;
}

/**
 * How load() treats a read in flight: "initial" reads only when nothing is
 * loaded or loading; "refresh" joins one in flight (timers, a player's grant
 * refresh); "reload" restarts it (after a change the in-flight read predates).
 */
export type LoadMode = "initial" | "refresh" | "reload";

interface Slot {
  key: string;
  ref: RefBody;
  options: ReadOptions;
  listeners: Map<() => void, ReadPolicy>;
  entry: ReadEntry;
  ctl?: AbortController;
  timer?: ReturnType<typeof setTimeout>;
}

const IDLE: ReadEntry = { read: null, loading: false, loaded: false };

/** rate_limited reads are retried this often before the error shows. */
const RATE_LIMIT_RETRIES = 3;

export function readKey(ref: RefBody, o: ReadOptions = {}): string {
  return JSON.stringify([ref.kind, ref.id, o.prefix ?? "", o.offset ?? 0, o.limit ?? 0, !!o.download, !!o.editor]);
}

/** The read with the upload at path replaced by file in place (appended when new, removed when null). */
export function withUpload(read: ReadResult | null, path: string, file: FileInfo | null): ReadResult {
  const all = read?.files ?? [];
  const at = all.findIndex((x) => x.upload && samePath(x.path, path));
  const files = all.filter((_, i) => i !== at);
  if (file) files.splice(at < 0 ? files.length : at, 0, file);
  return { access: "full", expires: 0, total: 0, offset: 0, limit: 0, ...read, files };
}

/**
 * The reads hooks share, per client: one request per item and options while
 * any hook shows it. set() updates every reader in place; a commit reloads
 * the item's reads and a processed upload is patched into its editor reads.
 * Readers may ask for polling while uploads process and a read before the
 * URLs expire; the slot runs one timer for all of them.
 */
export class ReadStore {
  private readonly slots = new Map<string, Slot>();

  constructor(private readonly client: ContentKitClient) {
    client.subscribe((c) => this.apply(c));
  }

  subscribe(key: string, ref: RefBody, options: ReadOptions, listener: () => void, policy: ReadPolicy = {}): () => void {
    let s = this.slots.get(key);
    if (!s) this.slots.set(key, (s = { key, ref, options, listeners: new Map(), entry: IDLE }));
    const slot = s;
    slot.listeners.set(listener, policy);
    this.schedule(slot);
    return () => {
      slot.listeners.delete(listener);
      // Kept across a StrictMode remount; dropped once no hook shows it.
      queueMicrotask(() => {
        if (this.slots.get(key) !== slot) return;
        if (slot.listeners.size) return this.schedule(slot);
        slot.ctl?.abort();
        clearTimeout(slot.timer);
        this.slots.delete(key);
      });
    };
  }

  snapshot(key: string): ReadEntry {
    return this.slots.get(key)?.entry ?? IDLE;
  }

  load(key: string, mode: LoadMode = "initial"): void {
    const s = this.slots.get(key);
    if (!s) return;
    if (mode === "initial" && (s.entry.loaded || s.ctl)) return;
    if (mode === "refresh" && s.ctl) return;
    s.ctl?.abort();
    clearTimeout(s.timer);
    const ctl = (s.ctl = new AbortController());
    this.update(s, { loading: true, error: undefined });
    this.fetch(s, ctl.signal).then(
      (read) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        const before = s.entry.read;
        this.update(s, { read, loading: false, loaded: true, error: undefined });
        this.schedule(s);
        if (s.options.editor && finished(before, read)) this.reloadViewers(s.ref);
      },
      (e) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        this.update(s, { read: null, loading: false, loaded: true, error: toContentKitError(e) });
      },
    );
  }

  /** Replaces the read for every hook showing it, without refetching. */
  set(key: string, read: ReadResult): void {
    const s = this.slots.get(key);
    if (!s) return;
    s.ctl?.abort();
    s.ctl = undefined;
    this.update(s, { read, loading: false, loaded: true, error: undefined });
    this.schedule(s);
  }

  // The client retries transport faults; a read rate limit waits it out here.
  private async fetch(s: Slot, signal: AbortSignal): Promise<ReadResult> {
    for (let attempt = 0; ; attempt++) {
      try {
        return await this.client.media.read(s.ref, { ...s.options, signal });
      } catch (e) {
        const err = toContentKitError(e);
        if (err.code !== "rate_limited" || attempt >= RATE_LIMIT_RETRIES || signal.aborted) throw err;
        await sleep(Math.min(30, err.retryAfter ?? 1) * 1000, signal);
      }
    }
  }

  // One timer per slot: the soonest of a poll (while uploads process) and the refresh before expiry.
  private schedule(s: Slot): void {
    clearTimeout(s.timer);
    s.timer = undefined;
    const read = s.entry.read;
    if (!read || s.ctl || !s.listeners.size) return;
    let delay: number | null = null;
    const polls = [...s.listeners.values()].map((p) => p.poll).filter((p): p is number => typeof p === "number" && p > 0);
    if (polls.length && processing(read)) delay = Math.min(...polls);
    if ([...s.listeners.values()].some((p) => p.refresh)) {
      const d = expiryDelay(read.expires);
      if (d !== null) delay = delay === null ? d : Math.min(delay, d);
    }
    if (delay !== null) s.timer = setTimeout(() => this.load(s.key, "refresh"), delay);
  }

  // Uploads finished processing: the item's viewer reads list new files and URLs.
  private reloadViewers(ref: RefBody): void {
    for (const [key, s] of this.slots) {
      if (!s.options.editor && s.ref.kind === ref.kind && s.ref.id === ref.id && (s.entry.loaded || s.ctl)) this.load(key, "reload");
    }
  }

  private apply(change: ContentKitChange): void {
    const { ref } = change;
    for (const [key, s] of this.slots) {
      if (s.ref.kind !== ref.kind || s.ref.id !== ref.id) continue;
      if (change.type === "media.committed") {
        if (s.entry.loaded || s.ctl) this.load(key, "reload");
        continue;
      }
      const read = s.entry.read;
      const { file } = change;
      if (!read) continue;
      // A viewer read cannot be patched: its URLs and public files change.
      if (!s.options.editor) {
        this.load(key, "reload");
        continue;
      }
      if (s.options.prefix && !file.path.startsWith(s.options.prefix)) continue;
      this.update(s, { read: withUpload(read, file.path, file) });
      // A read in flight may predate the processing.
      if (s.ctl) this.load(key, "reload");
    }
  }

  // A new entry per change: useSyncExternalStore compares snapshots by identity.
  private update(s: Slot, patch: Partial<ReadEntry>): void {
    s.entry = { ...s.entry, ...patch };
    for (const l of s.listeners.keys()) l();
  }
}

/** Some upload processing in before is done (or failed) in after. */
function finished(before: ReadResult | null, after: ReadResult): boolean {
  if (!before) return false;
  const now = new Map(after.files.filter((f) => f.upload).map((f) => [f.path, f]));
  return before.files.some((f) => isProcessing(f) && now.has(f.path) && !isProcessing(now.get(f.path)!));
}

const stores = new WeakMap<ContentKitClient, ReadStore>();

/** The client's shared read store. */
export function storeFor(client: ContentKitClient): ReadStore {
  let s = stores.get(client);
  if (!s) stores.set(client, (s = new ReadStore(client)));
  return s;
}

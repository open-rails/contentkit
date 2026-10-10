import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { FileInfo, ReadResult, RefBody } from "../client/generated/wire.js";
import type { ContentKitChange } from "../client/http.js";
import type { ReadOptions } from "../client/media/api.js";
import { samePath } from "../client/media/client.js";

export interface ReadEntry {
  read: ReadResult | null;
  loading: boolean;
  /** A read or set() has completed. */
  loaded: boolean;
  error?: ContentKitError;
}

interface Slot {
  ref: RefBody;
  options: ReadOptions;
  listeners: Set<() => void>;
  entry: ReadEntry;
  ctl?: AbortController;
}

const IDLE: ReadEntry = { read: null, loading: false, loaded: false };

export function readKey(ref: RefBody, o: ReadOptions = {}): string {
  return JSON.stringify([ref.kind, ref.id, o.prefix ?? "", o.offset ?? 0, o.limit ?? 0, !!o.download, !!o.editor]);
}

/** The read with the upload at path replaced by file (added, or removed when null). */
export function withUpload(read: ReadResult | null, path: string, file: FileInfo | null): ReadResult {
  const files = (read?.files ?? []).filter((x) => !(x.upload && samePath(x.path, path)));
  return { access: "full", expires: 0, total: 0, offset: 0, limit: 0, ...read, files: file ? [...files, file] : files };
}

/**
 * The reads hooks share, per client: one request per item and options while
 * any hook shows it; set() updates every reader in place; a processed upload
 * is patched into the item's editor reads.
 */
export class ReadStore {
  private readonly slots = new Map<string, Slot>();

  constructor(private readonly client: ContentKitClient) {
    client.subscribe((c) => this.apply(c));
  }

  subscribe(key: string, ref: RefBody, options: ReadOptions, listener: () => void): () => void {
    let s = this.slots.get(key);
    if (!s) this.slots.set(key, (s = { ref, options, listeners: new Set(), entry: IDLE }));
    const slot = s;
    slot.listeners.add(listener);
    return () => {
      slot.listeners.delete(listener);
      // Kept across a StrictMode remount; dropped once no hook shows it.
      queueMicrotask(() => {
        if (slot.listeners.size || this.slots.get(key) !== slot) return;
        slot.ctl?.abort();
        this.slots.delete(key);
      });
    };
  }

  snapshot(key: string): ReadEntry {
    return this.slots.get(key)?.entry ?? IDLE;
  }

  /** Fetches the read unless one is loaded or in flight; force refetches. */
  load(key: string, force = false): void {
    const s = this.slots.get(key);
    if (!s || (!force && (s.entry.loaded || s.ctl))) return;
    s.ctl?.abort();
    const ctl = (s.ctl = new AbortController());
    this.update(s, { loading: true, error: undefined });
    this.client.media.read(s.ref, { ...s.options, signal: ctl.signal }).then(
      (read) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        this.update(s, { read, loading: false, loaded: true, error: undefined });
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
  }

  private apply(change: ContentKitChange): void {
    if (change.type !== "media.processed") return;
    const { ref, file } = change;
    for (const s of this.slots.values()) {
      const read = s.entry.read;
      if (!read || !s.options.editor || s.ref.kind !== ref.kind || s.ref.id !== ref.id) continue;
      if (s.options.prefix && !file.path.startsWith(s.options.prefix)) continue;
      this.update(s, { read: withUpload(read, file.path, file) });
    }
  }

  // A new entry per change: useSyncExternalStore compares snapshots by identity.
  private update(s: Slot, patch: Partial<ReadEntry>): void {
    s.entry = { ...s.entry, ...patch };
    for (const l of s.listeners) l();
  }
}

const stores = new WeakMap<ContentKitClient, ReadStore>();

/** The client's shared read store. */
export function storeFor(client: ContentKitClient): ReadStore {
  let s = stores.get(client);
  if (!s) stores.set(client, (s = new ReadStore(client)));
  return s;
}

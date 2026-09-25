import type { Progress, UploadClient, UploadedFile, UploadState } from "./client.js";
import { UploadError } from "./errors.js";
import type { CommitFile, FileInfo, Op, RefBody } from "./wire.gen.js";

export type ItemStatus = "queued" | "uploading" | "uploaded" | "failed" | "committed";

export interface QueueItem {
  readonly id: string;
  readonly file: File;
  /** The manifest file name the commit inserts; default the file's name. */
  name: string;
  meta?: Record<string, unknown>;
  status: ItemStatus;
  progress?: Progress;
  error?: UploadError;
  result?: UploadedFile;
  /** Resumable multipart state while uploading or after a failure. */
  state?: UploadState;
  /**
   * Committed unattached for processing on upload (the host's
   * ProcessOnUpload): commit() attaches it, remove() discards it.
   */
  unattached?: boolean;
  /** The server's view of an unattached file while it processes: dims, hls, failed, progress. */
  processing?: FileInfo;
  /** Processing finished (derived, encoded or failed). */
  processed?: boolean;
}

export interface QueueSnapshot {
  readonly items: readonly QueueItem[];
  /** A rate, quota or permission refusal: no new uploads start until start(). */
  readonly blocked?: UploadError;
  /** Every item is uploaded (or committed) and none is in flight. */
  readonly ready: boolean;
}

export interface QueueOptions {
  ref: RefBody;
  /** Files uploading at once. Default 2. */
  concurrency?: number;
  /** Start uploads as files are added. Default true. */
  autoStart?: boolean;
  /** Called with every item state change, to persist resumable state. */
  onState?: (item: QueueItem, state: UploadState | null) => void;
  /** Milliseconds between processing polls of unattached files. Default 2000. */
  pollInterval?: number;
}

/**
 * An ordered file queue for one item: uploads in the background, keeps the
 * order the user arranges, and commits the uploaded files as inserts in that
 * order. Framework-free; the React hooks wrap it.
 */
export class UploadQueue {
  private items: QueueItem[] = [];
  private blocked?: UploadError;
  private running = new Map<string, AbortController>();
  private listeners = new Set<() => void>();
  private snap!: QueueSnapshot;
  private seq = 0;
  private started: boolean;
  /** In-flight unattached commits, by item id. */
  private staging = new Map<string, Promise<void>>();
  private poll?: ReturnType<typeof setTimeout>;
  private disposed = false;

  constructor(
    private readonly client: UploadClient,
    private readonly o: QueueOptions,
  ) {
    this.started = o.autoStart ?? true;
    this.publish();
  }

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getSnapshot = (): QueueSnapshot => this.snap;

  /** Queues files; state restores a multipart upload saved through onState before a reload. */
  add(
    files: Iterable<File>,
    opts: { name?: (f: File) => string; state?: (f: File) => UploadState | undefined } = {},
  ): QueueItem[] {
    const added = [...files].map(
      (file): QueueItem => ({
        id: `u${++this.seq}`,
        file,
        name: opts.name?.(file) ?? file.name,
        status: "queued",
        state: opts.state?.(file),
      }),
    );
    this.items.push(...added);
    this.changed();
    return added;
  }

  /** Moves an item to index (before commit). */
  move(id: string, index: number): void {
    const from = this.items.findIndex((i) => i.id === id);
    if (from < 0) return;
    const [item] = this.items.splice(from, 1);
    this.items.splice(Math.max(0, Math.min(index, this.items.length)), 0, item!);
    this.changed();
  }

  update(id: string, patch: Pick<Partial<QueueItem>, "name" | "meta">): void {
    const item = this.find(id);
    if (item?.unattached && patch.name && patch.name !== item.name) {
      void this.client.commit(this.o.ref, [{ op: "rename", name: item.name, to: patch.name }]).catch(() => {});
    }
    this.patch(id, patch);
  }

  /**
   * Removes an item, discarding its multipart upload if one is open, or its
   * unattached file (the server cancels its processing and deletes it).
   */
  remove(id: string): void {
    const item = this.find(id);
    if (!item) return;
    this.running.get(id)?.abort();
    if (item.state) void this.client.discard(item.state).catch(() => {});
    const staged = this.staging.get(id);
    if (item.unattached || staged) {
      void (staged ?? Promise.resolve())
        .then(() => this.client.commit(this.o.ref, [{ op: "remove", name: item.name }]))
        .catch(() => {});
    }
    this.o.onState?.(item, null);
    this.items = this.items.filter((i) => i.id !== id);
    this.changed();
  }

  /** Re-queues a failed item; a multipart upload resumes from its state. */
  retry(id: string): void {
    this.patch(id, { status: "queued", error: undefined });
  }

  /** Starts uploading (autoStart false) or lifts a block after a refusal. */
  start(): void {
    this.started = true;
    this.blocked = undefined;
    this.changed();
  }

  /** Stops starting uploads and pauses running ones (they stay resumable). */
  pause(): void {
    this.started = false;
    for (const c of this.running.values()) c.abort();
    this.changed();
  }

  /**
   * Commits every uploaded item, in queue order, appended to the manifest's
   * files: unattached ones are attached (no reprocessing), the rest inserted.
   * Returns the committed file order.
   */
  async commit(signal?: AbortSignal): Promise<CommitFile[]> {
    await Promise.all(this.staging.values());
    const ready = this.items.filter((i) => i.status === "uploaded");
    if (ready.length === 0) return [];
    let ops: Op[] = ready.map((i) => ({ op: "insert", name: i.name, original: i.result!.name, meta: i.meta }));
    if (ready.some((i) => i.unattached)) {
      // Attach in queue order; inserts join as unattached first so the order holds.
      ops = [
        ...ready.filter((i) => !i.unattached).map((i): Op => ({ op: "insert", name: i.name, original: i.result!.name, meta: i.meta, unattached: true })),
        ...ready.map((i): Op => ({ op: "attach", name: i.name, ...(i.meta ? { meta: i.meta } : {}) })),
      ];
    }
    const sources = Object.fromEntries(ready.map((i) => [i.result!.name, { file: i.file, type: i.result!.type }]));
    const files = await this.client.commit(this.o.ref, ops, { signal, sources });
    const original = new Map(files.map((f) => [f.name, f.original]));
    for (const i of ready) {
      // An original uploaded again at commit may have a new name (multipart).
      const name = original.get(i.name) ?? i.result!.name;
      this.set(i.id, { status: "committed", unattached: false, result: { ...i.result!, name } });
    }
    this.changed();
    return files.filter((f) => !f.unattached);
  }

  /** Aborts running uploads and stops polling; call on unmount. */
  dispose(): void {
    this.started = false;
    this.disposed = true;
    for (const c of this.running.values()) c.abort();
    clearTimeout(this.poll);
    this.listeners.clear();
  }

  /** Commits an uploaded file unattached, so the host processes it now. */
  private stage(id: string): void {
    const item = this.find(id);
    if (!item?.result) return;
    const op: Op = { op: "insert", name: item.name, original: item.result.name, meta: item.meta, unattached: true };
    const p = this.client
      .commit(this.o.ref, [op], { sources: { [item.result.name]: { file: item.file, type: item.result.type } } })
      .then(
        (files) => {
          const f = files.find((x) => x.name === item.name);
          const cur = this.find(id);
          if (cur && f) this.patch(id, { unattached: true, result: { ...cur.result!, name: f.original } });
          this.schedulePoll();
        },
        () => {}, // left uploaded: commit() inserts it
      )
      .finally(() => this.staging.delete(id));
    this.staging.set(id, p);
  }

  private schedulePoll(): void {
    if (this.poll || this.disposed) return;
    this.poll = setTimeout(() => {
      this.poll = undefined;
      void this.refresh().finally(() => {
        if (this.items.some((i) => i.unattached && !i.processed)) this.schedulePoll();
      });
    }, this.o.pollInterval ?? 2000);
  }

  /** Reads the processing state of unattached files still processing. */
  private async refresh(): Promise<void> {
    const pending = this.items.filter((i) => i.unattached && !i.processed);
    if (pending.length === 0) return;
    let files: FileInfo[];
    try {
      files = await this.client.files(this.o.ref, pending.map((i) => i.name));
    } catch {
      return;
    }
    const byName = new Map(files.map((f) => [f.name, f]));
    let changed = false;
    for (const i of pending) {
      const f = byName.get(i.name);
      if (!f) continue;
      const video = (f.type ?? "").startsWith("video/");
      const processed = !!f.failed || (video ? f.hls && !f.progress : (f.w ?? 0) > 0);
      changed = this.set(i.id, { processing: f, processed }) || changed;
    }
    if (changed) this.changed();
  }

  private find(id: string): QueueItem | undefined {
    return this.items.find((i) => i.id === id);
  }

  private patch(id: string, patch: Partial<QueueItem>): void {
    if (this.set(id, patch)) this.changed();
  }

  private set(id: string, patch: Partial<QueueItem>): boolean {
    const i = this.items.findIndex((x) => x.id === id);
    if (i >= 0) this.items[i] = { ...this.items[i]!, ...patch };
    return i >= 0;
  }

  private changed(): void {
    this.pump();
    this.publish();
    for (const fn of this.listeners) fn();
  }

  private publish(): void {
    this.snap = {
      items: [...this.items],
      blocked: this.blocked,
      ready:
        this.running.size === 0 &&
        this.items.length > 0 &&
        this.items.every((i) => i.status === "uploaded" || i.status === "committed"),
    };
  }

  private pump(): void {
    if (!this.started || this.blocked) return;
    const limit = this.o.concurrency ?? 2;
    for (const item of this.items) {
      if (this.running.size >= limit) return;
      if (item.status === "queued" && !this.running.has(item.id)) this.run(item.id);
    }
  }

  private run(id: string): void {
    const item = this.find(id)!;
    const ctl = new AbortController();
    this.running.set(id, ctl);
    this.set(id, { status: "uploading", progress: undefined, error: undefined });
    this.client
      .upload(item.file, {
        ref: this.o.ref,
        signal: ctl.signal,
        resume: item.state,
        onProgress: (progress) => this.patch(id, { progress }),
        onState: (state) => {
          this.patch(id, { state: state ?? undefined });
          const cur = this.find(id);
          if (cur) this.o.onState?.(cur, state);
        },
      })
      .then(
        (result) => {
          this.running.delete(id);
          this.patch(id, { status: "uploaded", result, state: undefined });
          if (result.processOnUpload) this.stage(id);
        },
        (e: unknown) => {
          this.running.delete(id);
          const error = e instanceof UploadError ? e : new UploadError("network", String(e));
          if (error.code === "aborted") {
            this.patch(id, { status: "queued" });
            return;
          }
          if (error.blocksQueue) {
            // Refused for every file: keep it queued behind the block.
            this.blocked = error;
            this.patch(id, { status: "queued", error });
            return;
          }
          this.patch(id, { status: "failed", error });
        },
      );
  }
}

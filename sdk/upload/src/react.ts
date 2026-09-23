import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { Progress, UploadClient, UploadedFile, UploadOptions } from "./client.js";
import { UploadError } from "./errors.js";
import { UploadQueue, type QueueOptions, type QueueSnapshot } from "./queue.js";

export interface UseUploadQueue extends QueueSnapshot {
  queue: UploadQueue;
  add: UploadQueue["add"];
  move: UploadQueue["move"];
  update: UploadQueue["update"];
  remove: UploadQueue["remove"];
  retry: UploadQueue["retry"];
  start: UploadQueue["start"];
  pause: UploadQueue["pause"];
  commit: UploadQueue["commit"];
}

/**
 * A file queue for one item (options are read once; key the component by
 * ref to switch items). Unmounting pauses running uploads.
 */
export function useUploadQueue(client: UploadClient, options: QueueOptions): UseUploadQueue {
  const [queue] = useState(() => new UploadQueue(client, { ...options, autoStart: false }));
  const autoStart = options.autoStart ?? true;
  useEffect(() => {
    if (autoStart) queue.start();
    return () => queue.pause();
  }, [queue, autoStart]);
  const snap = useSyncExternalStore(queue.subscribe, queue.getSnapshot, queue.getSnapshot);
  return {
    ...snap,
    queue,
    add: queue.add.bind(queue),
    move: queue.move.bind(queue),
    update: queue.update.bind(queue),
    remove: queue.remove.bind(queue),
    retry: queue.retry.bind(queue),
    start: queue.start.bind(queue),
    pause: queue.pause.bind(queue),
    commit: queue.commit.bind(queue),
  };
}

export interface UploadStatus {
  status: "idle" | "uploading" | "done" | "error";
  progress?: Progress;
  error?: UploadError;
  result?: UploadedFile;
}

export interface UseUpload extends UploadStatus {
  /** Uploads one file; with opts.slot it also commits the slot. */
  upload: (file: File, opts: Omit<UploadOptions, "signal" | "onProgress">) => Promise<UploadedFile>;
  cancel: () => void;
}

/** One upload at a time (a cover, an avatar); a new upload cancels the previous. */
export function useUpload(client: UploadClient): UseUpload {
  const [s, set] = useState<UploadStatus>({ status: "idle" });
  const ctl = useRef<AbortController | null>(null);
  useEffect(() => () => ctl.current?.abort(), []);

  const upload = useCallback<UseUpload["upload"]>(
    async (file, opts) => {
      ctl.current?.abort();
      const c = (ctl.current = new AbortController());
      set({ status: "uploading" });
      const o: UploadOptions = {
        ...opts,
        signal: c.signal,
        onProgress: (progress) => c === ctl.current && set({ status: "uploading", progress }),
      };
      try {
        const result = opts.slot ? await client.uploadSlot(file, { ...o, slot: opts.slot }) : await client.upload(file, o);
        if (c === ctl.current) set((prev) => ({ status: "done", progress: prev.progress, result }));
        return result;
      } catch (e) {
        const error = e instanceof UploadError ? e : new UploadError("network", String(e));
        if (c === ctl.current) set(error.code === "aborted" ? { status: "idle" } : { status: "error", error });
        throw error;
      }
    },
    [client],
  );
  const cancel = useCallback(() => ctl.current?.abort(), []);
  return { ...s, upload, cancel };
}

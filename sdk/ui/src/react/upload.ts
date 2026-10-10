import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { ratio, type AspectRatio } from "../client/aspect.js";
import type { ContentKitClient } from "../client/client.js";
import { centeredCrop, constrainCrop, editOf, rotation, sameEdit, toOriginal, type Rotation, type Size } from "../client/crop.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Crop, Edit, EncodeProgress, FileInfo } from "../client/generated/wire.js";
import type { Progress, PutOptions, UploadedFile, UploadOptions } from "../client/media/client.js";
import { encodeRemaining } from "../client/media/encode.js";
import { UploadQueue, type QueueOptions, type QueueSnapshot } from "../client/media/queue.js";
import { useContentKitClient } from "./context.js";

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

export interface UploadQueueOptions extends QueueOptions {
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

/**
 * A file queue for one item: another ref gets a fresh queue and the previous
 * one pauses (the other options are read once per queue). Unmounting pauses
 * running uploads.
 */
export function useUploadQueue({ client: own, ...options }: UploadQueueOptions): UseUploadQueue {
  const client = useContentKitClient(own);
  const key = `${options.ref.kind}/${options.ref.id}`;
  const open = () => ({ key, queue: new UploadQueue(client.media, { ...options, autoStart: false }) });
  const [current, setCurrent] = useState(open);
  if (current.key !== key) setCurrent(open());
  const { queue } = current;
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
  error?: ContentKitError;
  result?: UploadedFile & { file?: FileInfo };
}

export interface UseUpload extends UploadStatus {
  /**
   * Uploads one file; with put it also commits it to its path (put's edit,
   * meta and index) and waits until it is processed (result.file).
   */
  upload: (file: File, opts: Omit<UploadOptions, "signal" | "onProgress"> & { put?: PutOptions }) => Promise<UploadedFile & { file?: FileInfo }>;
  cancel: () => void;
}

/** One upload at a time (a cover, an inline image); a new upload cancels the previous. */
export function useUpload({ client: own }: { client?: ContentKitClient } = {}): UseUpload {
  const { media } = useContentKitClient(own);
  const [s, set] = useState<UploadStatus>({ status: "idle" });
  const ctl = useRef<AbortController | null>(null);
  useEffect(() => () => ctl.current?.abort(), []);

  const upload = useCallback<UseUpload["upload"]>(
    async (file, { put, ...opts }) => {
      ctl.current?.abort();
      const c = (ctl.current = new AbortController());
      set({ status: "uploading" });
      const o: UploadOptions = {
        ...opts,
        signal: c.signal,
        onProgress: (progress) => c === ctl.current && set({ status: "uploading", progress }),
      };
      try {
        let result: UploadedFile & { file?: FileInfo };
        if (put) {
          let up: UploadedFile | undefined;
          const f = await media.put(file, { ...o, ...put, onUploaded: (u) => (up = u) });
          result = { ...up!, file: f };
        } else result = await media.upload(file, o);
        if (c === ctl.current) set((prev) => ({ status: "done", progress: prev.progress, result }));
        return result;
      } catch (e) {
        const error = toContentKitError(e);
        if (c === ctl.current) set(error.code === "aborted" ? { status: "idle" } : { status: "error", error });
        throw error;
      }
    },
    [media],
  );
  const cancel = useCallback(() => ctl.current?.abort(), []);
  return { ...s, upload, cancel };
}

export interface UseCropOptions {
  /** The upload's size (an editor read's w and h). */
  source: Size;
  /** The edited image's "W:H", e.g. the public preset's aspect; "" or omitted is free. */
  aspect?: AspectRatio;
  /** The current edit to start from. */
  initial?: Edit | null;
}

export interface UseCrop {
  /** In original pixels, inside the source and at the aspect. */
  crop: Crop;
  rotate: Rotation;
  /** Ready for an edit or put op; null when nothing changes. Its identity changes only with its value. */
  edit: Edit | null;
  /** A rect in original pixels. */
  setCrop: (rect: Crop) => void;
  /** A rect on the image as displayed (after the rotation) at display size: a cropper's output. */
  setFromDisplay: (rect: Crop, display: Size) => void;
  /** Turns clockwise by deg (a multiple of 90); the crop keeps the aspect. */
  rotateBy: (deg: number) => void;
  reset: () => void;
}

/** Headless crop state for any cropper UI: a rect in original pixels, a rotation and the resulting edit. */
export function useCrop({ source: given, aspect: shape, initial }: UseCropOptions): UseCrop {
  const aspect = ratio(shape);
  const { width, height } = given;
  const source = useMemo(() => ({ width, height }), [width, height]);
  const start = (): [Crop, Rotation] => {
    const rotate = rotation(initial?.rotate ?? 0);
    const c = initial?.crop;
    return [c ? constrainCrop(c, source, aspect, rotate) : centeredCrop(source, aspect, rotate), rotate];
  };
  const [state, setState] = useState(start);
  const [crop, rotate] = state;
  const set = useCallback(
    (next: [Crop, Rotation] | ((s: [Crop, Rotation]) => [Crop, Rotation])) =>
      setState((s) => {
        const n = typeof next === "function" ? next(s) : next;
        return sameEdit({ crop: n[0], rotate: n[1] }, { crop: s[0], rotate: s[1] }) ? s : n;
      }),
    [],
  );
  const setCrop = useCallback((r: Crop) => set(([, rot]) => [constrainCrop(r, source, aspect, rot), rot]), [set, source, aspect]);
  const setFromDisplay = useCallback(
    (r: Crop, display: Size) => set(([, rot]) => [constrainCrop(toOriginal(r, display, source, rot), source, aspect, rot), rot]),
    [set, source, aspect],
  );
  const rotateBy = useCallback(
    (deg: number) =>
      set(([c, rot]) => {
        const next = rotation(rot + deg);
        return [constrainCrop(c, source, aspect, next), next];
      }),
    [set, source, aspect],
  );
  const reset = useCallback(() => set([centeredCrop(source, aspect), 0]), [set, source, aspect]);
  const last = useRef<Edit | null>(null);
  const edit = useMemo(() => {
    const e = editOf(crop, source, rotate);
    return sameEdit(e, last.current) ? last.current : (last.current = e);
  }, [crop, source, rotate]);
  return { crop, rotate, edit, setCrop, setFromDisplay, rotateBy, reset };
}

export interface UseEncodeProgress {
  progress?: EncodeProgress;
  /** Seconds left, counting down each second between reports; undefined while unknown. */
  remaining?: number;
}

/**
 * A pending video file's encode progress from the read API (`files[i].progress`),
 * with the ETA counted down locally between polls.
 */
export function useEncodeProgress(progress?: EncodeProgress | null): UseEncodeProgress {
  const [base, setBase] = useState(() => ({ at: progress?.at, received: Date.now() }));
  if (progress?.at !== base.at) setBase({ at: progress?.at, received: Date.now() });
  const [now, setNow] = useState(() => Date.now());
  const ticking = !!progress?.eta && !progress.stalled;
  useEffect(() => {
    if (!ticking) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [ticking, base]);
  return { progress: progress ?? undefined, remaining: encodeRemaining(progress, base.received, Math.max(now, base.received)) };
}

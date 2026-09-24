import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { Progress, SlotUploadOptions, UploadClient, UploadedFile, UploadOptions } from "./client.js";
import { centeredCrop, constrainCrop, editOf, rotation, toOriginal, type Rotation, type Size } from "./crop.js";
import { UploadError } from "./errors.js";
import { UploadQueue, type QueueOptions, type QueueSnapshot } from "./queue.js";
import type { Crop, Edit } from "./wire.gen.js";

export {
  editOutput,
  useSlotCrop,
  useSlotImage,
  type SlotCropMode,
  type SlotCropOptions,
  type SlotCropState,
  type SlotImageOptions,
  type UseSlotCrop,
  type UseSlotImage,
} from "./slot-react.js";

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
  /** Uploads one file; with opts.slot or opts.inline it also commits the slot or inline image (opts.edit crops a slot). */
  upload: (file: File, opts: Omit<UploadOptions, "signal" | "onProgress"> & { edit?: SlotUploadOptions["edit"] }) => Promise<UploadedFile>;
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
        const result = opts.slot
          ? await client.uploadSlot(file, { ...o, slot: opts.slot, edit: opts.edit })
          : opts.inline
            ? await client.uploadInline(file, o)
            : await client.upload(file, o);
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

export interface UseCropOptions {
  /** The original's size (read API dims). */
  source: Size;
  /** The edited image's width/height, e.g. the slot's aspect. */
  aspect?: number;
  /** The current edit to start from. */
  initial?: Edit | null;
}

export interface UseCrop {
  /** In original pixels, inside the source and at the aspect. */
  crop: Crop;
  rotate: Rotation;
  /** Ready for client.edit or setSlotFromFile; null when nothing changes. */
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
export function useCrop({ source, aspect, initial }: UseCropOptions): UseCrop {
  const start = (): [Crop, Rotation] => {
    const rotate = rotation(initial?.rotate ?? 0);
    const c = initial?.crop;
    return [c ? constrainCrop(c, source, aspect, rotate) : centeredCrop(source, aspect, rotate), rotate];
  };
  const [state, set] = useState(start);
  const [crop, rotate] = state;
  const setCrop = useCallback((r: Crop) => set(([, rot]) => [constrainCrop(r, source, aspect, rot), rot]), [source, aspect]);
  const setFromDisplay = useCallback(
    (r: Crop, display: Size) => set(([, rot]) => [constrainCrop(toOriginal(r, display, source, rot), source, aspect, rot), rot]),
    [source, aspect],
  );
  const rotateBy = useCallback(
    (deg: number) =>
      set(([c, rot]) => {
        const next = rotation(rot + deg);
        return [constrainCrop(c, source, aspect, next), next];
      }),
    [source, aspect],
  );
  const reset = useCallback(() => set([centeredCrop(source, aspect), 0]), [source, aspect]);
  const edit = useMemo(() => editOf(crop, source, rotate), [crop, source, rotate]);
  return { crop, rotate, edit, setCrop, setFromDisplay, rotateBy, reset };
}

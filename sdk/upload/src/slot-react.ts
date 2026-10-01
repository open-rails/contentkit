import { ratio, type AspectRatio } from "./aspect.js";
import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { samePath, stem, type Progress, type UploadClient } from "./client.js";
import { centeredCrop, constrainCrop, editedSize, rotation, sameEdit, type Size } from "./crop.js";
import { UploadError } from "./errors.js";
import { decodeImage, isAnimatedImage, type CropSource } from "./image.js";
import { publicGeneration, publicRenditions, reloadPublic, subscribePublic, type PublicImage } from "./public.js";
import { useRead } from "./read-react.js";
import type { Rendition } from "./rendition.js";
import type { Edit, FileInfo, ReadResult, RefBody } from "./wire.gen.js";

const asError = (e: unknown) => (e instanceof UploadError ? e : new UploadError("network", String(e)));

/** Bumps after every reloadPublic: key images by it to show refetched public files. */
export function usePublicGeneration(): number {
  return useSyncExternalStore(subscribePublic, publicGeneration, publicGeneration);
}

/** Refetches a public preset's files after a change; nothing without a preset. */
export const reloadImage = (image: PublicImage | null | undefined) => reloadPublic(publicRenditions(image).map((r) => r.url));

export interface SlotImageOptions {
  ref: RefBody;
  /** The upload path, e.g. "cover" or "avatar". */
  path: string;
  /** The public preset showing the upload. */
  image?: PublicImage | null;
  /** An editor read of the item the host already has; skips the fetch. */
  read?: ReadResult | null;
}

export interface UseSlotImage {
  /** The upload at path as an editor reads it; null when there is none. */
  file: FileInfo | null;
  /** The public preset's files (always served: a missing one is the kind's default). */
  renditions: Rendition[];
  /** "W:H": the preset's, else "1:1". */
  aspect: AspectRatio;
  loading: boolean;
  error?: UploadError;
  reload: () => void;
  /** Replaces the upload after a save without refetching. */
  set: (file: FileInfo | null) => void;
}

/** An upload path's state (an editor read) and its public image. */
export function useSlotImage(client: UploadClient | null | undefined, o: SlotImageOptions): UseSlotImage {
  const r = useRead(client, o.ref, { editor: true, prefix: stem(o.path), read: o.read });
  const read = r.read;
  const file = read?.files.find((f) => f.upload && samePath(f.path, o.path)) ?? null;
  const { path } = o;
  const update = r.set;
  const set = useCallback(
    (f: FileInfo | null) => {
      const files = (read?.files ?? []).filter((x) => !(x.upload && samePath(x.path, path)));
      update({ access: "full", preview_limit: 0, expires: 0, total: 0, offset: 0, limit: 0, ...read, files: f ? [...files, f] : files });
    },
    [read, path, update],
  );
  const renditions = useMemo(() => publicRenditions(o.image), [o.image]);
  return { file, renditions, aspect: o.image?.aspect || "1:1", loading: r.loading, error: r.error, reload: r.reload, set };
}

export type SlotCropMode = "new" | "recrop";

/** edit: null is the centred crop at the aspect, unrotated. */
export type SlotCropState =
  | { status: "idle" }
  | { status: "decoding" }
  | { status: "cropping"; source: CropSource; edit: Edit | null; mode: SlotCropMode }
  | { status: "saving"; source: CropSource; edit: Edit | null; mode: SlotCropMode; progress?: Progress; rendering?: boolean }
  | { status: "done"; file: FileInfo }
  | { status: "error"; error: UploadError; source?: CropSource; edit?: Edit | null; mode?: SlotCropMode };

export interface SlotCropOptions {
  ref: RefBody;
  /** The upload path, e.g. "avatar". */
  path: string;
  /** The current upload (useSlotImage's file): its path, size and edit for recrop(). */
  file?: FileInfo | null;
  /** The output's "W:H"; default "1:1". */
  aspect?: AspectRatio;
  /** "reject": refuse animated images before uploading (the preset's Image.Animation). */
  animation?: "reject";
  /** The public preset to refetch after a save. */
  image?: PublicImage | null;
  onSaved?: (file: FileInfo) => void;
  /** Replaces decodeImage (tests, custom decoders). */
  decode?: (file: File) => Promise<CropSource>;
  /** How long save() waits for the worker to render. Default 120 s. */
  renderTimeout?: number;
  /** Every failed decode or save (aborts excepted); the state shows it too. */
  onError?: (e: UploadError, operation: "slot.decode" | "slot.save") => void;
}

export type UseSlotCrop = SlotCropState & {
  aspect: AspectRatio;
  /** The output's size in source pixels (crop, then rotation) while a source is open. */
  cropped?: Size;
  /** Opens a picked file for cropping. */
  pick: (file: File) => Promise<void>;
  /** Re-crops the committed upload; false when there is none. */
  canRecrop: boolean;
  recrop: () => Promise<void>;
  setEdit: (e: Edit | null) => void;
  /** Uploads the file and puts it with the edit (new), or edits the upload (recrop); then waits for the render. */
  save: (edit?: Edit | null) => Promise<FileInfo | undefined>;
  /** Aborts a save and closes the source. */
  cancel: () => void;
};

/** The edit's output size: its crop (or the centred crop at aspect), turned. */
export function editOutput(source: Size, edit: Edit | null | undefined, shape: AspectRatio): Size {
  const aspect = ratio(shape);
  const rot = rotation(edit?.rotate ?? 0);
  const c = edit?.crop ? constrainCrop(edit.crop, source, aspect, rot) : centeredCrop(source, aspect, rot);
  return editedSize({ width: c.w, height: c.h }, rot);
}

/** pick → crop → save → done, and recrop() → crop → save for an existing upload. */
export function useSlotCrop(client: UploadClient, o: SlotCropOptions): UseSlotCrop {
  const [s, setS] = useState<SlotCropState>({ status: "idle" });
  const cur = useRef(s);
  cur.current = s;
  const ctl = useRef<AbortController | null>(null);
  const picks = useRef(0);
  const opts = useRef(o);
  opts.current = o;
  const aspect = o.aspect ?? "1:1";

  const set = useCallback((next: SlotCropState) => {
    const prev = cur.current;
    const src = "source" in prev ? prev.source : undefined;
    const keep = "source" in next ? next.source : undefined;
    if (src && src !== keep) src.revoke?.();
    cur.current = next;
    setS(next);
  }, []);

  useEffect(
    () => () => {
      ctl.current?.abort();
      const c = cur.current;
      if ("source" in c) c.source?.revoke?.();
    },
    [],
  );

  const open = useCallback(
    async (load: () => Promise<CropSource>, mode: SlotCropMode, edit: Edit | null) => {
      ctl.current?.abort();
      ctl.current = null;
      const n = ++picks.current;
      set({ status: "decoding" });
      try {
        const source = await load();
        if (n !== picks.current) return source.revoke?.();
        set({ status: "cropping", source, edit, mode });
      } catch (e) {
        if (n !== picks.current) return;
        const error = e instanceof UploadError ? e : new UploadError("decode", String(e));
        set({ status: "error", error });
        opts.current.onError?.(error, "slot.decode");
      }
    },
    [set],
  );

  const pick = useCallback(
    (file: File) =>
      open(async () => {
        if (opts.current.animation === "reject" && (await isAnimatedImage(file)))
          throw new UploadError("animation_not_allowed", "animated images are not allowed here", 422);
        return (opts.current.decode ?? decodeImage)(file);
      }, "new", null),
    [open],
  );

  const canRecrop = !!o.file && (o.file.size ?? 0) > 0 && !!o.file.type.startsWith("image/");
  const recrop = useCallback(async () => {
    const f = opts.current.file;
    if (!f) return;
    await open(() => client.editorView(opts.current.ref, f.path), "recrop", f.edit ?? null);
  }, [client, open]);

  const setEdit = useCallback(
    (edit: Edit | null) => {
      const c = cur.current;
      if (c.status === "cropping") {
        if (!sameEdit(c.edit, edit)) set({ ...c, edit });
      } else if (c.status === "error" && c.source && c.mode) set({ status: "cropping", source: c.source, edit, mode: c.mode });
    },
    [set],
  );

  const save = useCallback(
    async (next?: Edit | null) => {
      if (next !== undefined) setEdit(next);
      const c = cur.current;
      if (!(c.status === "cropping" || (c.status === "error" && c.source && c.mode))) return undefined;
      const { source, mode } = c as { source: CropSource; mode: SlotCropMode };
      const edit = c.edit ?? null;
      const { ref, path, renderTimeout: timeout } = opts.current;
      const a = (ctl.current = new AbortController());
      const saving = (progress?: Progress) =>
        a === ctl.current && set({ status: "saving", source, edit, mode, progress, rendering: progress?.phase === "processing" });
      saving();
      try {
        let file: FileInfo;
        if (mode === "new") file = await client.put(source.file!, { ref, path, edit, signal: a.signal, timeout, onProgress: saving });
        else {
          const at = opts.current.file?.path ?? path;
          await client.commit(ref, [{ op: "edit", path: at, ...(edit ? { edit } : {}) }], { signal: a.signal });
          saving({ phase: "processing", loaded: 0, total: 0 });
          file = await client.waitFor(ref, at, { signal: a.signal, timeout });
        }
        await reloadImage(opts.current.image);
        if (a !== ctl.current) return undefined;
        set({ status: "done", file });
        opts.current.onSaved?.(file);
        return file;
      } catch (e) {
        if (a !== ctl.current) return undefined;
        const error = asError(e);
        set(error.code === "aborted" ? { status: "cropping", source, edit, mode } : { status: "error", error, source, edit, mode });
        if (error.code !== "aborted") opts.current.onError?.(error, "slot.save");
        return undefined;
      }
    },
    [client, set, setEdit],
  );

  const cancel = useCallback(() => {
    picks.current++;
    ctl.current?.abort();
    ctl.current = null;
    set({ status: "idle" });
  }, [set]);

  const cropped = useMemo(() => ("source" in s && s.source ? editOutput(s.source, s.edit, aspect) : undefined), [s, aspect]);
  return { ...s, aspect, cropped, pick, canRecrop, recrop, setEdit, save, cancel };
}

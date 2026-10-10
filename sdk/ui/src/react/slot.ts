import { useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ratio, type AspectRatio } from "../client/aspect.js";
import type { ContentKitClient } from "../client/client.js";
import { centeredCrop, constrainCrop, editedSize, rotation, sameEdit, type Size } from "../client/crop.js";
import { ContentKitError, toContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, PresetRule, ReadResult, RefBody } from "../client/generated/wire.js";
import { decodeImage, isAnimatedImage, type CropSource } from "../client/image.js";
import { samePath, stem, type Progress } from "../client/media/client.js";
import { presetFor } from "../client/media/rules.js";
import { publicRenditions, type PublicPreset } from "../client/public.js";
import type { Rendition } from "../client/rendition.js";
import { useContentKitClient, useReadScope } from "./context.js";
import { usePresets } from "./public.js";
import { useMediaRead } from "./read.js";
import { withUpload } from "./store.js";

export interface SlotImageOptions {
  ref: RefBody;
  /** The upload path, e.g. "cover" or "avatar". */
  path: string;
  /** The public preset showing the upload. */
  image?: PublicPreset | null;
  /** An editor read of the item the host already has; skips the fetch. */
  read?: ReadResult | null;
  /** Overrides the provider's client; without one only a supplied read shows. */
  client?: ContentKitClient | null;
}

export interface UseSlotImage {
  /** The upload at path as an editor reads it; null when there is none. */
  file: FileInfo | null;
  /** The current published files; empty until the worker publishes them. */
  renditions: Rendition[];
  /** "W:H": the image's, else its public preset's (GET /media/presets), else "1:1". */
  aspect: AspectRatio;
  /** The path's public preset (aspect, widths, min_width) once the presets load. */
  rule?: PresetRule;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Replaces the upload after a save without refetching. */
  set: (file: FileInfo | null) => void;
}

/** An upload path's state (an editor read) and its public image. */
export function useSlotImage(o: SlotImageOptions): UseSlotImage {
  const r = useMediaRead(o.ref, { editor: true, prefix: stem(o.path), read: o.read, client: o.client });
  const { presets } = usePresets({ client: o.client });
  const rule = presetFor(presets, o.ref.kind, o.path, o.image?.preset);
  const read = r.read;
  const file = read?.files.find((f) => f.upload && samePath(f.path, o.path)) ?? null;
  const { path } = o;
  const update = r.set;
  const set = useCallback((f: FileInfo | null) => update(withUpload(read, path, f)), [read, path, update]);
  const renditions = useMemo(() => {
    if (!read) return publicRenditions(o.image);
    const published = read.public?.find((p) => samePath(p.from, path) && (!o.image || p.preset === o.image.preset));
    return publicRenditions(published);
  }, [read, o.image, path]);
  return { file, renditions, aspect: o.image?.aspect || rule?.aspect || "1:1", rule, loading: r.loading, error: r.error, reload: r.reload, set };
}

export type SlotCropMode = "new" | "recrop";

/** edit: null is the centred crop at the aspect, unrotated. */
export type SlotCropState =
  | { status: "idle" }
  | { status: "decoding" }
  | { status: "cropping"; source: CropSource; edit: Edit | null; mode: SlotCropMode }
  | { status: "saving"; source: CropSource; edit: Edit | null; mode: SlotCropMode; progress?: Progress; rendering?: boolean }
  | { status: "done"; file: FileInfo }
  | { status: "error"; error: ContentKitError; source?: CropSource; edit?: Edit | null; mode?: SlotCropMode };

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
  onSaved?: (file: FileInfo) => void;
  /** Replaces decodeImage (tests, custom decoders). */
  decode?: (file: File) => Promise<CropSource>;
  /** How long save() waits for the worker to render. Default 120 s. */
  renderTimeout?: number;
  /** Every failed decode or save (aborts excepted); the state shows it too. */
  onError?: (e: ContentKitError, operation: "slot.decode" | "slot.save") => void;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
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
export function useSlotCrop(o: SlotCropOptions): UseSlotCrop {
  const { media } = useContentKitClient(o.client);
  const scope = useReadScope();
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
        const error = e instanceof ContentKitError ? e : new ContentKitError("decode", e instanceof Error ? e.message : String(e), { cause: e });
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
          throw new ContentKitError("animation_not_allowed", "animated images are not allowed here", { status: 422 });
        return (opts.current.decode ?? decodeImage)(file);
      }, "new", null),
    [open],
  );

  const canRecrop = !!o.file && !o.file.staged && (o.file.size ?? 0) > 0 && o.file.type.startsWith("image/");
  const recrop = useCallback(async () => {
    const f = opts.current.file;
    if (!f) return;
    await open(() => media.editorView(opts.current.ref, f.path), "recrop", f.edit ?? null);
  }, [media, open]);

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
        if (mode === "new") file = await media.put(source.file!, { ref, path, edit, signal: a.signal, timeout, onProgress: saving });
        else {
          const at = opts.current.file?.path ?? path;
          await media.commit(ref, [{ op: "edit", path: at, ...(edit ? { edit } : {}) }], { signal: a.signal });
          if (a !== ctl.current) return undefined;
          saving({ phase: "processing", loaded: 0, total: 0 });
          file = await media.waitFor(ref, at, { signal: a.signal, timeout });
        }
        if (a !== ctl.current) return undefined;
        set({ status: "done", file });
        opts.current.onSaved?.(file);
        return file;
      } catch (e) {
        if (a !== ctl.current) return undefined;
        const error = toContentKitError(e);
        set(error.code === "aborted" ? { status: "cropping", source, edit, mode } : { status: "error", error, source, edit, mode });
        if (error.code !== "aborted") opts.current.onError?.(error, "slot.save");
        return undefined;
      }
    },
    [media, set, setEdit],
  );

  const cancel = useCallback(() => {
    picks.current++;
    ctl.current?.abort();
    ctl.current = null;
    set({ status: "idle" });
  }, [set]);

  // Cancel before old work can reach the current scope's editor callbacks.
  useLayoutEffect(() => cancel, [scope, media, cancel]);

  const cropped = useMemo(() => ("source" in s && s.source ? editOutput(s.source, s.edit, aspect) : undefined), [s, aspect]);
  return { ...s, aspect, cropped, pick, canRecrop, recrop, setEdit, save, cancel };
}

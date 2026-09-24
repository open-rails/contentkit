import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Progress, UploadClient } from "./client.js";
import { centeredCrop, constrainCrop, editedSize, rotation, sameEdit, type Size } from "./crop.js";
import { slotError, UploadError } from "./errors.js";
import { decodeImage, type CropSource } from "./image.js";
import { hasOriginal, manifestAspect, slotSources, type SlotSources } from "./srcset.js";
import type { Edit, RefBody, SlotManifest } from "./wire.gen.js";

export interface SlotImageOptions {
  ref: RefBody;
  slot: string;
  /** A manifest the host already has (e.g. from its own API); skips the fetch. */
  manifest?: SlotManifest | null;
}

export interface UseSlotImage extends SlotSources {
  manifest: SlotManifest | null;
  aspect: number;
  loading: boolean;
  error?: UploadError;
  reload: () => void;
  /** Replaces the manifest after a save without refetching. */
  set: (m: SlotManifest) => void;
}

/** A slot's manifest and `srcset`; fetched with getSlot unless given. */
export function useSlotImage(client: UploadClient | null | undefined, o: SlotImageOptions): UseSlotImage {
  const given = o.manifest;
  const key = `${o.ref.kind}/${o.ref.id}/${o.ref.version ?? ""}#${o.slot}`;
  const [state, setState] = useState<{ key: string; manifest: SlotManifest | null; loading: boolean; error?: UploadError }>({
    key,
    manifest: null,
    loading: given === undefined && !!client,
  });
  const [tick, setTick] = useState(0);
  const ref = useRef(o.ref);
  ref.current = o.ref;

  useEffect(() => {
    if (given !== undefined || !client) return;
    const ctl = new AbortController();
    setState((s) => ({ key, manifest: s.key === key ? s.manifest : null, loading: true }));
    client.getSlot(ref.current, o.slot, ctl.signal).then(
      (manifest) => setState({ key, manifest, loading: false }),
      (e) => {
        if (ctl.signal.aborted) return;
        const error = e instanceof UploadError ? e : new UploadError("network", String(e));
        setState({ key, manifest: null, loading: false, error: error.code === "not_found" ? undefined : error });
      },
    );
    return () => ctl.abort();
  }, [client, key, o.slot, given, tick]);

  const manifest = given !== undefined ? given : state.key === key ? state.manifest : null;
  const reload = useCallback(() => setTick((t) => t + 1), []);
  const set = useCallback((m: SlotManifest) => setState({ key, manifest: m, loading: false }), [key]);
  return {
    ...slotSources(manifest),
    manifest,
    aspect: manifestAspect(manifest, 1),
    loading: given === undefined && state.loading,
    error: given === undefined ? state.error : undefined,
    reload,
    set,
  };
}

export type SlotCropMode = "new" | "recrop";

/** edit: null is the centred crop at the slot's aspect, unrotated. */
export type SlotCropState =
  | { status: "idle" }
  | { status: "decoding" }
  | { status: "cropping"; source: CropSource; edit: Edit | null; mode: SlotCropMode }
  | { status: "saving"; source: CropSource; edit: Edit | null; mode: SlotCropMode; progress?: Progress; rendering?: boolean }
  | { status: "done"; manifest: SlotManifest }
  | { status: "error"; error: UploadError; source?: CropSource; edit?: Edit | null; mode?: SlotCropMode };

export interface SlotCropOptions {
  ref: RefBody;
  slot: string;
  /** The slot's current manifest: its aspect, source size and edit for recrop(). */
  manifest?: SlotManifest | null;
  /** Width / height of the output; default the manifest's aspect, else 1. */
  aspect?: number;
  onSaved?: (m: SlotManifest) => void;
  /** Replaces decodeImage (tests, custom decoders). */
  decode?: (file: File) => Promise<CropSource>;
  /** How long save() waits for the server to encode the outputs. Default 60 s. */
  renderTimeout?: number;
  /** Every failed decode or save (aborts excepted); the state shows it too. */
  onError?: (e: UploadError, operation: "slot.decode" | "slot.save") => void;
}

export type UseSlotCrop = SlotCropState & {
  aspect: number;
  /** The output's size in source pixels (crop, then rotation) while a source is open. */
  cropped?: Size;
  /** Opens a picked file for cropping. */
  pick: (file: File) => Promise<void>;
  /** Re-crops the slot's committed original; false when the manifest has none. */
  canRecrop: boolean;
  recrop: () => Promise<void>;
  setEdit: (e: Edit | null) => void;
  /** Uploads the original and commits the edit (new), or re-renders the original (recrop). */
  save: (edit?: Edit | null) => Promise<SlotManifest | undefined>;
  /** Aborts a save and closes the source. */
  cancel: () => void;
};

/** The edit's output size: its crop (or the centred crop at aspect), turned. */
export function editOutput(source: Size, edit: Edit | null | undefined, aspect: number): Size {
  const rot = rotation(edit?.rotate ?? 0);
  const c = edit?.crop ? constrainCrop(edit.crop, source, aspect, rot) : centeredCrop(source, aspect, rot);
  return editedSize({ width: c.w, height: c.h }, rot);
}

/** pick → crop → save → done, and recrop() → crop → save for an existing slot image. */
export function useSlotCrop(client: UploadClient, o: SlotCropOptions): UseSlotCrop {
  const [s, setS] = useState<SlotCropState>({ status: "idle" });
  const cur = useRef(s);
  cur.current = s;
  const ctl = useRef<AbortController | null>(null);
  const picks = useRef(0);
  const opts = useRef(o);
  opts.current = o;
  const aspect = o.aspect ?? manifestAspect(o.manifest, 1);

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

  const pick = useCallback((file: File) => open(() => (opts.current.decode ?? decodeImage)(file), "new", null), [open]);

  const canRecrop = hasOriginal(o.manifest);
  const recrop = useCallback(async () => {
    const m = opts.current.manifest;
    if (!m || !hasOriginal(m)) return;
    const { ref, slot } = opts.current;
    await open(
      async () => {
        const blob = await client.getSlotOriginal(ref, slot);
        return (opts.current.decode ?? decodeImage)(new File([blob], slot, { type: blob.type }));
      },
      "recrop",
      m.edit ?? null,
    );
  }, [client, open]);

  const setEdit = useCallback(
    (edit: Edit | null) => {
      const c = cur.current;
      if (c.status === "cropping") {
        if (!sameEdit(c.edit, edit)) set({ ...c, edit });
      }
      else if (c.status === "error" && c.source && c.mode) set({ status: "cropping", source: c.source, edit, mode: c.mode });
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
      const { ref, slot } = opts.current;
      const a = (ctl.current = new AbortController());
      set({ status: "saving", source, edit, mode });
      try {
        let manifest =
          mode === "new"
            ? (
                await client.uploadSlot(source.file!, {
                  ref,
                  slot,
                  edit,
                  signal: a.signal,
                  onProgress: (progress) => a === ctl.current && set({ status: "saving", source, edit, mode, progress }),
                })
              ).manifest
            : await client.editSlot(ref, slot, edit, a.signal);
        if (manifest.pending) {
          if (a === ctl.current) set({ status: "saving", source, edit, mode, rendering: true });
          manifest = await client.waitForSlot(ref, slot, { signal: a.signal, timeout: opts.current.renderTimeout });
        }
        if (manifest.pending) throw new UploadError("render_timeout", "the slot is still rendering");
        if (manifest.error) throw slotError(manifest);
        if (a !== ctl.current) return undefined;
        set({ status: "done", manifest });
        opts.current.onSaved?.(manifest);
        return manifest;
      } catch (e) {
        if (a !== ctl.current) return undefined;
        const error = e instanceof UploadError ? e : new UploadError("network", String(e));
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

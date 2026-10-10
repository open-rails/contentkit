import { useCallback, useEffect, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, RefBody } from "../client/generated/wire.js";
import type { CropSource } from "../client/image.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "./context.js";

export interface EditorCropOptions {
  /** save() also waits until the worker rendered the edit. Default false (an editor read shows it processing). */
  wait?: boolean;
  /** With the item's uploads after each save. */
  onSaved?: (files: FileInfo[]) => void;
  /** Every failed load or save; default the provider's. */
  onError?: ContentKitErrorHandler;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

/** source is the upload's editor view and its oriented size; edit its current edit. */
export type EditorCropState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "cropping"; source: CropSource; edit: Edit | null }
  | { status: "saving"; source: CropSource; edit: Edit | null }
  | { status: "done"; files: FileInfo[] }
  | { status: "error"; error: ContentKitError; source?: CropSource; edit?: Edit | null };

export type UseEditorCrop = EditorCropState & {
  /** Commits the edit (null removes it); resolves with the item's uploads, or undefined on failure. */
  save: (edit: Edit | null) => Promise<FileInfo[] | undefined>;
  /** Loads the editor view again after a failure. */
  retry: () => void;
};

/**
 * Re-crops a kept upload: while path is set, its editor view (the original,
 * oriented, unedited) loads for a cropper; save() commits an edit op. Nothing
 * is uploaded. Clear path to close.
 */
export function useEditorCrop(ref: RefBody, path: string | null | undefined, o: EditorCropOptions = {}): UseEditorCrop {
  const { media } = useContentKitClient(o.client);
  const report = useErrorReporter(o.onError);
  const key = path ? `${ref.kind}/${ref.id}/${path}` : "";
  const [attempt, setAttempt] = useState(0);
  const [s, setS] = useState<{ key: string; state: EditorCropState }>({ key: "", state: { status: "idle" } });
  const opts = useRef({ o, ref, path });
  opts.current = { o, ref, path };

  useEffect(() => {
    const { ref, path } = opts.current;
    if (!path) return;
    const ctl = new AbortController();
    setS({ key, state: { status: "loading" } });
    media.editorView(ref, path, { signal: ctl.signal }).then(
      (v) => setS({ key, state: { status: "cropping", source: { url: v.url, width: v.width, height: v.height }, edit: v.edit ?? null } }),
      (e: unknown) => {
        if (ctl.signal.aborted) return;
        const error = toContentKitError(e);
        setS({ key, state: { status: "error", error } });
        report(error, "crop.load", path.split("/").pop());
      },
    );
    return () => ctl.abort();
  }, [media, key, attempt, report]);

  const current = s.key === key ? s.state : ({ status: key ? "loading" : "idle" } as EditorCropState);
  const now = useRef(current);
  now.current = current;

  const save = useCallback(
    async (edit: Edit | null) => {
      const c = now.current;
      const { ref, path, o } = opts.current;
      const source = "source" in c ? c.source : undefined;
      if (!path || !source) return undefined;
      setS({ key, state: { status: "saving", source, edit } });
      try {
        const files = await media.commit(ref, [{ op: "edit", path, ...(edit ? { edit } : {}) }]);
        if (o.wait) await media.waitFor(ref, path);
        setS({ key, state: { status: "done", files } });
        o.onSaved?.(files);
        return files;
      } catch (e) {
        const error = toContentKitError(e);
        setS({ key, state: { status: "error", error, source, edit } });
        report(error, "crop.save", path.split("/").pop());
        return undefined;
      }
    },
    [media, key, report],
  );
  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return { ...current, save, retry };
}

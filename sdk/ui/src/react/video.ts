import { useCallback, useEffect, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, Op, ReadResult, RefBody } from "../client/generated/wire.js";
import { samePath, type Progress } from "../client/media/client.js";
import { useContentKitClient } from "./context.js";
import { useMediaRead } from "./read.js";
import { withUpload } from "./store.js";

const asError = toContentKitError;
const refKey = (ref: RefBody) => `${ref.kind}/${ref.id}`;

export interface VideoImagesOptions {
  ref: RefBody;
  /** The video upload path. Default "source". */
  video?: string;
  /** The poster upload path, grabbed from the video's frames. Default "poster". */
  poster?: string;
  /** An editor read of the item the host already has; skips the fetch. */
  read?: ReadResult | null;
  /** Overrides the provider's client; without one only a supplied read shows. */
  client?: ContentKitClient | null;
}

export interface UseVideoImages {
  /** The video upload (its duration and size once probed); null without one. */
  video: FileInfo | null;
  /** The poster upload: a frame ({t} or {auto}) or an uploaded image; null without one. */
  poster: FileInfo | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Replaces the poster after a save without refetching. */
  set: (poster: FileInfo) => void;
}

/** A video item's video and poster uploads (an editor read). */
export function useVideoImages(o: VideoImagesOptions): UseVideoImages {
  const r = useMediaRead(o.ref, { editor: true, read: o.read, client: o.client });
  const find = (p: string) => r.read?.files.find((f) => f.upload && samePath(f.path, p)) ?? null;
  const posterPath = o.poster ?? "poster";
  const { read, set: update } = r;
  const set = useCallback((poster: FileInfo) => update(withUpload(read, posterPath, poster)), [read, posterPath, update]);
  return { video: find(o.video ?? "source"), poster: find(posterPath), loading: r.loading, error: r.error, reload: r.reload, set };
}

export interface FrameStripOptions {
  ref: RefBody;
  /** The video upload's path (a read's, with its extension). */
  path?: string;
  /** Seconds; nothing is fetched until it is known. */
  duration?: number;
  /** Frames across the video. Default 8. */
  count?: number;
  /** Pixels. Default 160. */
  width?: number;
  /** The first failed grab (the strip keeps going without it). */
  onError?: (e: ContentKitError) => void;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

export interface StripFrame {
  time: number;
  /** An object URL once fetched. */
  url?: string;
}

/**
 * Evenly spaced frames for coarse browsing, fetched one at a time (the frame
 * endpoint allows two grabs at once per host), revoked on change or unmount.
 */
export function useFrameStrip(o: FrameStripOptions): StripFrame[] {
  const { media } = useContentKitClient(o.client);
  const count = o.count ?? 8;
  const width = o.width ?? 160;
  const duration = o.duration ?? 0;
  const key = `${refKey(o.ref)}#${o.path ?? ""}#${duration}#${count}#${width}`;
  const [state, setState] = useState<{ key: string; frames: StripFrame[] }>({ key: "", frames: [] });
  const ref = useRef(o.ref);
  ref.current = o.ref;
  const onError = useRef(o.onError);
  onError.current = o.onError;
  useEffect(() => {
    if (!(duration > 0) || !o.path) return;
    const path = o.path;
    const ctl = new AbortController();
    let reported = false;
    const times = Array.from({ length: count }, (_, i) => round3(((i + 0.5) / count) * duration));
    const urls: string[] = [];
    setState({ key, frames: times.map((time) => ({ time })) });
    void (async () => {
      for (const [i, time] of times.entries()) {
        try {
          const blob = await media.getFrame(ref.current, path, time, width, ctl.signal);
          if (ctl.signal.aborted) return;
          const url = URL.createObjectURL(blob);
          urls.push(url);
          setState((s) => (s.key === key ? { key, frames: s.frames.map((f, j) => (j === i ? { ...f, url } : f)) } : s));
        } catch (e) {
          if (ctl.signal.aborted) return;
          if (!reported) onError.current?.(asError(e));
          reported = true;
        }
      }
    })();
    return () => {
      ctl.abort();
      urls.forEach((u) => URL.revokeObjectURL(u));
    };
  }, [media, key, duration, count, width, o.path]);
  return state.key === key ? state.frames : [];
}

export interface VideoFrameOptions {
  ref: RefBody;
  /** The video upload's path; nothing is fetched without it. */
  path?: string;
  /** Seconds; undefined fetches nothing. */
  time?: number;
  /** Pixels. Default 640. */
  width?: number;
  /** Debounce while scrubbing, ms. Default 120. */
  delay?: number;
  onError?: (e: ContentKitError) => void;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

export interface UseVideoFrame {
  /** The latest fetched frame; kept while the next one loads. */
  url?: string;
  /** The time url shows. */
  time?: number;
  loading: boolean;
  error?: ContentKitError;
}

/** The exact frame at time, debounced while it changes. */
export function useVideoFrame(o: VideoFrameOptions): UseVideoFrame {
  const { media } = useContentKitClient(o.client);
  const width = o.width ?? 640;
  const delay = o.delay ?? 120;
  const [state, setState] = useState<UseVideoFrame>({ loading: false });
  const ref = useRef(o.ref);
  ref.current = o.ref;
  const last = useRef<string | undefined>(undefined);
  const onError = useRef(o.onError);
  onError.current = o.onError;
  const key = refKey(o.ref);
  useEffect(() => {
    if (o.time === undefined || !o.path) return;
    const time = o.time;
    const path = o.path;
    const ctl = new AbortController();
    setState((s) => ({ ...s, loading: true, error: undefined }));
    const t = setTimeout(() => {
      media.getFrame(ref.current, path, time, width, ctl.signal).then(
        (blob) => {
          if (ctl.signal.aborted) return;
          if (last.current) URL.revokeObjectURL(last.current);
          const url = (last.current = URL.createObjectURL(blob));
          setState({ url, time, loading: false });
        },
        (e) => {
          if (ctl.signal.aborted) return;
          setState((s) => ({ ...s, loading: false, error: asError(e) }));
          onError.current?.(asError(e));
        },
      );
    }, delay);
    return () => {
      clearTimeout(t);
      ctl.abort();
    };
  }, [media, key, o.path, o.time, width, delay]);
  useEffect(
    () => () => {
      if (last.current) URL.revokeObjectURL(last.current);
    },
    [],
  );
  return state;
}

export type VideoSaveState =
  | { status: "idle" }
  | { status: "saving"; progress?: Progress; rendering?: boolean }
  | { status: "error"; error: ContentKitError };

export interface VideoPosterOptions {
  ref: RefBody;
  /** The poster upload path. Default "poster". */
  path?: string;
  /** With the processed poster upload after each save. */
  onSaved?: (poster: FileInfo) => void;
  /** Polling limit for the render, ms. Default 120000. */
  timeout?: number;
  /** Every failed save: the request, the render or the wait. */
  onError?: (e: ContentKitError) => void;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

export interface UseVideoPoster {
  state: VideoSaveState;
  /** A frame at time (seconds), cropped by edit in the frame's pixels (the video's w×h). */
  saveFrame: (time: number, edit?: Edit | null) => Promise<FileInfo | undefined>;
  /** An uploaded image, cropped by edit in its own pixels. */
  saveUpload: (image: Blob, edit?: Edit | null) => Promise<FileInfo | undefined>;
  /** The worker's choice of frame. */
  saveAuto: () => Promise<FileInfo | undefined>;
  /**
   * A frame of another item's video upload (at path, time seconds) uploaded
   * as the poster image, cropped by edit in the frame's pixels.
   */
  saveFrameFrom: (video: RefBody, path: string, time: number, edit?: Edit | null) => Promise<FileInfo | undefined>;
  reset: () => void;
}

/** Headless poster selection: a frame op or an uploaded image, then the wait for the worker to render it. */
export function useVideoPoster(o: VideoPosterOptions): UseVideoPoster {
  const { media } = useContentKitClient(o.client);
  const [state, setState] = useState<VideoSaveState>({ status: "idle" });
  const opts = useRef(o);
  opts.current = o;
  const run = useCallback(async (save: (onProgress: (p: Progress) => void) => Promise<FileInfo>) => {
    setState({ status: "saving" });
    try {
      const f = await save((progress) => setState({ status: "saving", progress, rendering: progress.phase === "processing" }));
      setState({ status: "idle" });
      opts.current.onSaved?.(f);
      return f;
    } catch (e) {
      const error = asError(e);
      setState({ status: "error", error });
      opts.current.onError?.(error);
      return undefined;
    }
  }, []);
  const frame = useCallback(
    (op: Omit<Op, "op" | "path">) =>
      run(async (onProgress) => {
        const { ref, path = "poster", timeout } = opts.current;
        await media.commit(ref, [{ op: "frame", path, ...op }]);
        onProgress({ phase: "processing", loaded: 0, total: 0 });
        return media.waitFor(ref, path, { timeout });
      }),
    [media, run],
  );
  return {
    state,
    saveFrame: useCallback((time, edit) => frame({ t: round3(time), ...(edit ? { edit } : {}) }), [frame]),
    saveUpload: useCallback(
      (image, edit) => {
        const { ref, path = "poster", timeout } = opts.current;
        return run((onProgress) => media.put(image, { ref, path, edit, timeout, onProgress }));
      },
      [media, run],
    ),
    saveAuto: useCallback(() => frame({ auto: true }), [frame]),
    saveFrameFrom: useCallback(
      (video, path, time, edit) => {
        const { ref, path: to = "poster", timeout } = opts.current;
        return run(async (onProgress) => {
          const still = await media.getFrame(video, path, round3(time));
          // GET /frame answers a JPEG.
          const file = new File([still], "frame.jpg", { type: "image/jpeg" });
          return media.put(file, { ref, path: to, edit, timeout, onProgress });
        });
      },
      [media, run],
    ),
    reset: useCallback(() => setState({ status: "idle" }), []),
  };
}

export const round3 = (v: number) => Math.round(v * 1000) / 1000;

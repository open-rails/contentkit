import { useCallback, useEffect, useRef, useState } from "react";
import type { Progress, UploadClient } from "./client.js";
import { slotError, UploadError } from "./errors.js";
import type { Edit, RefBody, VideoImages } from "./wire.gen.js";

const asError = (e: unknown) => (e instanceof UploadError ? e : new UploadError("network", String(e)));
const refKey = (ref: RefBody) => `${ref.kind}/${ref.id}/${ref.version ?? ""}`;

export interface VideoImagesOptions {
  ref: RefBody;
  /** The video file (multi-file kinds); default the first video. */
  file?: string;
  /** Images the host already has; skips the fetch. */
  images?: VideoImages | null;
}

export interface UseVideoImages {
  images: VideoImages | null;
  loading: boolean;
  error?: UploadError;
  reload: () => void;
  /** Replaces the images after a save without refetching. */
  set: (v: VideoImages) => void;
}

/** A video item's poster and selection (client.getVideoImages unless given). */
export function useVideoImages(client: UploadClient | null | undefined, o: VideoImagesOptions): UseVideoImages {
  const given = o.images;
  const key = `${refKey(o.ref)}#${o.file ?? ""}`;
  const [state, setState] = useState<{ key: string; images: VideoImages | null; loading: boolean; error?: UploadError }>({
    key,
    images: null,
    loading: given === undefined && !!client,
  });
  const [tick, setTick] = useState(0);
  const ref = useRef(o.ref);
  ref.current = o.ref;
  useEffect(() => {
    if (given !== undefined || !client) return;
    const ctl = new AbortController();
    setState((s) => ({ key, images: s.key === key ? s.images : null, loading: true }));
    client.getVideoImages(ref.current, o.file, ctl.signal).then(
      (images) => setState({ key, images, loading: false }),
      (e) => !ctl.signal.aborted && setState({ key, images: null, loading: false, error: asError(e) }),
    );
    return () => ctl.abort();
  }, [client, key, o.file, given, tick]);
  const images = given !== undefined ? given : state.key === key ? state.images : null;
  return {
    images,
    loading: given === undefined && state.loading,
    error: given === undefined ? state.error : undefined,
    reload: useCallback(() => setTick((t) => t + 1), []),
    set: useCallback((v: VideoImages) => setState({ key, images: v, loading: false }), [key]),
  };
}

export interface FrameStripOptions {
  ref: RefBody;
  file?: string;
  /** Seconds; nothing is fetched until it is known. */
  duration?: number;
  /** Frames across the video. Default 8. */
  count?: number;
  /** Pixels. Default 160. */
  width?: number;
  /** The first failed grab (the strip keeps going without it). */
  onError?: (e: UploadError) => void;
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
export function useFrameStrip(client: UploadClient, o: FrameStripOptions): StripFrame[] {
  const count = o.count ?? 8;
  const width = o.width ?? 160;
  const duration = o.duration ?? 0;
  const key = `${refKey(o.ref)}#${o.file ?? ""}#${duration}#${count}#${width}`;
  const [state, setState] = useState<{ key: string; frames: StripFrame[] }>({ key: "", frames: [] });
  const ref = useRef(o.ref);
  ref.current = o.ref;
  const onError = useRef(o.onError);
  onError.current = o.onError;
  useEffect(() => {
    if (!(duration > 0)) return;
    const ctl = new AbortController();
    let reported = false;
    const times = Array.from({ length: count }, (_, i) => round3(((i + 0.5) / count) * duration));
    const urls: string[] = [];
    setState({ key, frames: times.map((time) => ({ time })) });
    void (async () => {
      for (const [i, time] of times.entries()) {
        try {
          const blob = await client.getFrame(ref.current, time, { file: o.file, width, signal: ctl.signal });
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
  }, [client, key, duration, count, width, o.file]);
  return state.key === key ? state.frames : [];
}

export interface VideoFrameOptions {
  ref: RefBody;
  file?: string;
  /** Seconds; undefined fetches nothing. */
  time?: number;
  /** Pixels. Default 640. */
  width?: number;
  /** Debounce while scrubbing, ms. Default 120. */
  delay?: number;
  onError?: (e: UploadError) => void;
}

export interface UseVideoFrame {
  /** The latest fetched frame; kept while the next one loads. */
  url?: string;
  /** The time url shows. */
  time?: number;
  loading: boolean;
  error?: UploadError;
}

/** The exact frame at time, debounced while it changes. */
export function useVideoFrame(client: UploadClient, o: VideoFrameOptions): UseVideoFrame {
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
    if (o.time === undefined) return;
    const time = o.time;
    const ctl = new AbortController();
    setState((s) => ({ ...s, loading: true, error: undefined }));
    const t = setTimeout(() => {
      client.getFrame(ref.current, time, { file: o.file, width, signal: ctl.signal }).then(
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
  }, [client, key, o.file, o.time, width, delay]);
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
  | { status: "error"; error: UploadError };

export interface VideoPosterOptions {
  ref: RefBody;
  file?: string;
  /** With the rendered images after each save. */
  onSaved?: (v: VideoImages) => void;
  /** Polling limit for the render, ms. Default 120000. */
  timeout?: number;
  /** Every failed save: the request, the render or the wait. */
  onError?: (e: UploadError) => void;
}

export interface UseVideoPoster {
  state: VideoSaveState;
  /** A frame at time (seconds), cropped by edit in the frame's pixels (VideoInfo w×h). */
  saveFrame: (time: number, edit?: Edit | null) => Promise<VideoImages | undefined>;
  /** An uploaded image, cropped by edit in its own pixels. */
  saveUpload: (image: Blob, edit?: Edit | null) => Promise<VideoImages | undefined>;
  /** Back to the automatic frame. */
  saveAuto: () => Promise<VideoImages | undefined>;
  reset: () => void;
}

/** Headless poster selection: save, then wait for the worker and image job to render. */
export function useVideoPoster(client: UploadClient, o: VideoPosterOptions): UseVideoPoster {
  const [state, setState] = useState<VideoSaveState>({ status: "idle" });
  const opts = useRef(o);
  opts.current = o;
  const run = useCallback(
    async (save: (onProgress: (p: Progress) => void) => Promise<VideoImages>) => {
      const { ref, file, timeout } = opts.current;
      setState({ status: "saving" });
      try {
        let v = await save((progress) => setState({ status: "saving", progress }));
        if (v.poster.pending) {
          setState({ status: "saving", rendering: true });
          v = await waitFor(client, ref, file, (x) => !x.poster.pending, timeout);
        }
        if (v.poster.error) throw slotError(v.poster);
        setState({ status: "idle" });
        opts.current.onSaved?.(v);
        return v;
      } catch (e) {
        const error = asError(e);
        setState({ status: "error", error });
        opts.current.onError?.(error);
        return undefined;
      }
    },
    [client],
  );
  return {
    state,
    saveFrame: useCallback(
      (time, edit) => run(() => client.setVideoPoster(opts.current.ref, { source: "frame", time: round3(time), file: opts.current.file, edit })),
      [client, run],
    ),
    saveUpload: useCallback(
      (image, edit) => run((onProgress) => client.uploadVideoPoster(image, { ref: opts.current.ref, edit, onProgress })),
      [client, run],
    ),
    saveAuto: useCallback(() => run(() => client.setVideoPoster(opts.current.ref, { source: "auto", file: opts.current.file })), [client, run]),
    reset: useCallback(() => setState({ status: "idle" }), []),
  };
}

async function waitFor(client: UploadClient, ref: RefBody, file: string | undefined, done: (v: VideoImages) => boolean, timeout = 120_000): Promise<VideoImages> {
  const until = Date.now() + timeout;
  for (;;) {
    await new Promise((r) => setTimeout(r, 1000));
    const v = await client.getVideoImages(ref, file);
    if (done(v)) return v;
    if (Date.now() >= until) throw new UploadError("render_timeout", "rendering is taking too long; check back later");
  }
}

export const round3 = (v: number) => Math.round(v * 1000) / 1000;

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitError, toContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, Op, ReadResult, RefBody } from "../client/generated/wire.js";
import { samePath, type Progress } from "../client/media/client.js";
import { classifyHlsError, hlsConfig, type PlaybackError } from "../client/playback.js";
import { spriteCueAt, type SpriteCue } from "../client/player.js";
import { useContentKitClient } from "./context.js";
import { hlsLoading } from "./gallery.js";
import { useSprite } from "./player.js";
import { useMediaRead } from "./read.js";
import { withUpload } from "./store.js";

const asError = toContentKitError;

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
  /** The video's playable HLS ladder: its folder in the read's `hls`; undefined until one is encoded. */
  dir?: string;
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
  const video = find(o.video ?? "source");
  const dir = video ? read?.hls?.find((d) => read.files.some((f) => !f.upload && f.from === video.path && f.path.startsWith(d))) : undefined;
  return { video, ...(dir ? { dir } : {}), poster: find(posterPath), loading: r.loading, error: r.error, reload: r.reload, set };
}

export interface FrameStripOptions {
  ref: RefBody;
  /** The video's HLS ladder folder (useVideoImages' `dir`); nothing loads without it. */
  dir?: string;
  /** Seconds; nothing shows until it is known. */
  duration?: number;
  /** Frames across the video. Default 8. */
  count?: number;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

/** A tile of the seek sprite and the size of the sheet it cuts. */
export interface SpriteTile extends SpriteCue {
  sheetWidth: number;
  sheetHeight: number;
}

export interface StripFrame {
  time: number;
  /** The sprite tile showing at time, once the sprite loads. */
  tile?: SpriteTile;
}

/** Evenly spaced frames for coarse browsing: tiles of the ladder's seek sprite (sprite.vtt), one sheet for all. */
export function useFrameStrip(o: FrameStripOptions): StripFrame[] {
  const { media } = useContentKitClient(o.client);
  const count = o.count ?? 8;
  const duration = o.duration ?? 0;
  const sprite = useSprite(o.dir ? `${media.hlsBase(o.ref, o.dir)}sprite.vtt?editor` : null, media.hls);
  return useMemo(() => {
    if (!(duration > 0)) return [];
    return Array.from({ length: count }, (_, i) => {
      const time = round3(((i + 0.5) / count) * duration);
      const cue = sprite && spriteCueAt(sprite.cues, time);
      return cue ? { time, tile: { ...cue, sheetWidth: sprite.width, sheetHeight: sprite.height } } : { time };
    });
  }, [sprite, count, duration]);
}

export interface VideoFrameOptions {
  ref: RefBody;
  /** The video's HLS ladder folder (useVideoImages' `dir`); nothing loads without it. */
  dir?: string;
  /** Seconds; the video seeks there. */
  time?: number;
  onError?: (e: PlaybackError) => void;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

export interface UseVideoFrame {
  /** Attach to the `<video>` that shows the frame. */
  ref: (el: HTMLVideoElement | null) => void;
  /** The time the video shows; undefined while it loads or seeks. */
  time?: number;
  loading: boolean;
  error?: PlaybackError;
  /** The shown frame as a PNG at the rendition's size. */
  capture: () => Promise<Blob>;
}

/** Short side of the rendition frames are decoded from: the largest up to it. */
const FRAME_RUNG = 1080;

/**
 * The frame a player shows at time, which a frame op at that time grabs,
 * decoded in the browser from the ladder's largest rendition up to 1080p
 * (hls.js; no server work). The `<video>` shows it; capture() copies it for
 * a crop.
 */
export function useVideoFrame(o: VideoFrameOptions): UseVideoFrame {
  const { media } = useContentKitClient(o.client);
  const [el, setEl] = useState<HTMLVideoElement | null>(null);
  const [shown, setShown] = useState<{ src: string; time: number }>();
  const [failed, setFailed] = useState<{ src: string; error: PlaybackError }>();
  const src = o.dir ? `${media.hlsBase(o.ref, o.dir)}master.m3u8?editor` : undefined;
  const want = useRef(o.time);
  want.current = o.time;
  const onError = useRef(o.onError);
  onError.current = o.onError;
  useEffect(() => {
    if (!el || !src) return;
    let dead = false;
    let destroy = () => {};
    const fail = (error: PlaybackError) => {
      if (dead) return;
      dead = true;
      destroy();
      setFailed({ src, error });
      onError.current?.(error);
    };
    void import("hls.js").then(
      ({ default: Hls }) => {
        if (dead) return;
        if (!Hls.isSupported()) return fail({ kind: "unsupported", code: "mse" });
        const hls = new Hls({ ...hlsConfig, ...hlsLoading(() => media.hls), autoStartLoad: false, capLevelToPlayerSize: false, maxBufferLength: 4 });
        destroy = () => hls.destroy();
        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          const sides = hls.levels.map((l) => Math.min(l.width || l.height, l.height || l.width));
          const under = sides.filter((side) => side <= FRAME_RUNG);
          hls.currentLevel = sides.indexOf(under.length ? Math.max(...under) : Math.min(...sides));
          hls.startLoad(want.current ?? 0);
        });
        hls.on(Hls.Events.ERROR, (_, d) => {
          if (d.fatal) fail(classifyHlsError(d));
        });
        hls.attachMedia(el);
        hls.loadSource(src);
      },
      () => fail({ kind: "unknown", code: "hls.js" }),
    );
    return () => {
      dead = true;
      destroy();
    };
  }, [el, src, media]);
  useEffect(() => {
    if (!el || !src || o.time === undefined) return;
    const time = o.time;
    const seek = () => {
      el.currentTime = time;
    };
    const seeked = () => {
      if (want.current === time) setShown({ src, time });
    };
    el.addEventListener("seeked", seeked);
    if (el.readyState >= 1) seek();
    else el.addEventListener("loadedmetadata", seek, { once: true });
    return () => {
      el.removeEventListener("seeked", seeked);
      el.removeEventListener("loadedmetadata", seek);
    };
  }, [el, src, o.time]);
  const capture = useCallback(async () => {
    if (!el?.videoWidth || el.readyState < 2) throw new ContentKitError("decode", "no frame is shown yet");
    const canvas = document.createElement("canvas");
    canvas.width = el.videoWidth;
    canvas.height = el.videoHeight;
    canvas.getContext("2d")!.drawImage(el, 0, 0);
    return new Promise<Blob>((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new ContentKitError("decode", "the frame could not be copied"))), "image/png"));
  }, [el]);
  const error = failed && failed.src === src ? failed.error : undefined;
  const time = shown && shown.src === src && shown.time === o.time ? shown.time : undefined;
  return { ref: setEl, ...(time !== undefined ? { time } : {}), loading: !!src && o.time !== undefined && time === undefined && !error, ...(error ? { error } : {}), capture };
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
   * A frame of another item's video upload (at path, time seconds): the
   * worker grabs it once, cropped by edit in that video's pixels.
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
      (video, path, time, edit) =>
        frame({ t: round3(time), from: { kind: video.kind, id: video.id, ...(video.version ? { version: video.version } : {}), path }, ...(edit ? { edit } : {}) }),
      [frame],
    ),
    reset: useCallback(() => setState({ status: "idle" }), []),
  };
}

export const round3 = (v: number) => Math.round(v * 1000) / 1000;

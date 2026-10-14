import { useCallback, useEffect, useRef, useState } from "react";
import { parseSpriteVtt, playedBetween, type PlaySample, type SpriteCue } from "../client/player.js";
import type { HlsPlayerOptions } from "./gallery.js";

/** What the controls show about a `<video>`, kept current from its events. */
export interface MediaState {
  time: number;
  duration: number;
  /** The end of the buffered range holding the playhead. */
  buffered: number;
  paused: boolean;
  ended: boolean;
  volume: number;
  muted: boolean;
  rate: number;
  /** The decoded frame's width / height, once known. */
  aspect?: number;
}

const idle: MediaState = { time: 0, duration: 0, buffered: 0, paused: true, ended: false, volume: 1, muted: false, rate: 1 };

const MEDIA_EVENTS = ["timeupdate", "durationchange", "loadedmetadata", "progress", "play", "pause", "ended", "volumechange", "ratechange", "seeking", "seeked", "resize", "emptied"];

function snapshot(el: HTMLVideoElement): MediaState {
  const time = el.currentTime || 0;
  let buffered = 0;
  for (let i = 0; i < el.buffered.length; i++)
    if (el.buffered.start(i) <= time + 0.5 && time <= el.buffered.end(i)) buffered = el.buffered.end(i);
  const duration = Number.isFinite(el.duration) ? el.duration : 0;
  return {
    time,
    duration,
    buffered,
    paused: el.paused,
    ended: el.ended,
    volume: el.volume,
    muted: el.muted,
    rate: el.playbackRate,
    aspect: el.videoWidth > 0 && el.videoHeight > 0 ? el.videoWidth / el.videoHeight : undefined,
  };
}

export function useMediaState(el: HTMLVideoElement | null): MediaState {
  const [state, setState] = useState(idle);
  useEffect(() => {
    if (!el) return;
    const sync = () => setState(snapshot(el));
    sync();
    for (const e of MEDIA_EVENTS) el.addEventListener(e, sync);
    return () => {
      for (const e of MEDIA_EVENTS) el.removeEventListener(e, sync);
    };
  }, [el]);
  return state;
}

type WebkitDocument = Document & { webkitFullscreenElement?: Element | null; webkitExitFullscreen?: () => void };
type WebkitElement = HTMLElement & { webkitRequestFullscreen?: () => void };
type WebkitVideo = HTMLVideoElement & { webkitEnterFullscreen?: () => void; webkitExitFullscreen?: () => void; webkitDisplayingFullscreen?: boolean };

const fullscreenElement = () => {
  if (typeof document === "undefined") return null;
  const d = document as WebkitDocument;
  return d.fullscreenElement ?? d.webkitFullscreenElement ?? null;
};

/**
 * Fullscreen on the player's root, so the controls and menus come along;
 * iPhone Safari, which only fullscreens a video, gets its native player.
 */
export function useFullscreen(root: HTMLElement | null, el: HTMLVideoElement | null) {
  const [active, setActive] = useState(false);
  useEffect(() => {
    if (!root) return;
    const v = el as WebkitVideo | null;
    const sync = () => setActive(fullscreenElement() === root || !!v?.webkitDisplayingFullscreen);
    document.addEventListener("fullscreenchange", sync);
    document.addEventListener("webkitfullscreenchange", sync);
    v?.addEventListener("webkitbeginfullscreen", sync);
    v?.addEventListener("webkitendfullscreen", sync);
    return () => {
      document.removeEventListener("fullscreenchange", sync);
      document.removeEventListener("webkitfullscreenchange", sync);
      v?.removeEventListener("webkitbeginfullscreen", sync);
      v?.removeEventListener("webkitendfullscreen", sync);
    };
  }, [root, el]);
  const r = root as WebkitElement | null;
  const v = el as WebkitVideo | null;
  const supported = !!(r?.requestFullscreen || r?.webkitRequestFullscreen || v?.webkitEnterFullscreen);
  const toggle = useCallback(() => {
    if (!r) return;
    const d = document as WebkitDocument;
    if (fullscreenElement() === r) return void (d.exitFullscreen ? d.exitFullscreen().catch(() => {}) : d.webkitExitFullscreen?.());
    if (v?.webkitDisplayingFullscreen) return v.webkitExitFullscreen?.();
    if (r.requestFullscreen) return void r.requestFullscreen().catch(() => {});
    if (r.webkitRequestFullscreen) return r.webkitRequestFullscreen();
    v?.webkitEnterFullscreen?.();
  }, [r, v]);
  return { active, supported, toggle };
}

export function usePictureInPicture(el: HTMLVideoElement | null) {
  const [active, setActive] = useState(false);
  useEffect(() => {
    if (!el) return;
    const on = () => setActive(true);
    const off = () => setActive(false);
    el.addEventListener("enterpictureinpicture", on);
    el.addEventListener("leavepictureinpicture", off);
    return () => {
      el.removeEventListener("enterpictureinpicture", on);
      el.removeEventListener("leavepictureinpicture", off);
    };
  }, [el]);
  const supported = typeof document !== "undefined" && !!document.pictureInPictureEnabled && !!el && !el.disablePictureInPicture;
  const toggle = useCallback(() => {
    if (!el) return;
    if (document.pictureInPictureElement === el) void document.exitPictureInPicture().catch(() => {});
    else void el.requestPictureInPicture?.().catch(() => {});
  }, [el]);
  return { active, supported, toggle };
}

const isSubtitles = (t: TextTrack) => t.kind === "subtitles" || t.kind === "captions";

/**
 * The cues showing now on the video's chosen subtitle track. The track is
 * kept "hidden" so the browser draws nothing and the player places the
 * cues itself, clear of its controls.
 */
export function useActiveCues(el: HTMLVideoElement | null): VTTCue[] {
  const [cues, setCues] = useState<VTTCue[]>([]);
  useEffect(() => {
    if (!el) return;
    const tracks = el.textTracks;
    let track: TextTrack | null = null;
    const show = () => setCues(track?.activeCues ? (Array.from(track.activeCues) as VTTCue[]) : []);
    const pick = () => {
      const next = Array.from(tracks).find((t) => isSubtitles(t) && t.mode !== "disabled") ?? null;
      if (next?.mode === "showing") next.mode = "hidden";
      if (next === track) return show();
      track?.removeEventListener("cuechange", show);
      track = next;
      track?.addEventListener("cuechange", show);
      show();
    };
    pick();
    tracks.addEventListener?.("change", pick);
    tracks.addEventListener?.("addtrack", pick);
    tracks.addEventListener?.("removetrack", pick);
    return () => {
      track?.removeEventListener("cuechange", show);
      tracks.removeEventListener?.("change", pick);
      tracks.removeEventListener?.("addtrack", pick);
      tracks.removeEventListener?.("removetrack", pick);
    };
  }, [el]);
  return cues;
}

/** A seek sprite: its cues and the size of the sheet they cut. */
export interface Sprite {
  cues: SpriteCue[];
  width: number;
  height: number;
}

type XhrSetup = HlsPlayerOptions["xhrSetup"];
const sprites = new Map<string, Promise<Sprite | null>>();

function fetchText(url: string, setup: XhrSetup): Promise<string> {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    const send = () => {
      if (xhr.readyState === 0) xhr.open("GET", url, true);
      xhr.onload = () => resolve(xhr.status === 200 ? xhr.responseText : "");
      xhr.onerror = () => resolve("");
      xhr.send();
    };
    Promise.resolve(setup?.(xhr, url)).then(send, () => resolve(""));
  });
}

/** A ladder's sprite.vtt and its sheet, fetched once per URL (through xhrSetup, like the playlists). */
export function loadSprite(vtt: string, setup?: XhrSetup): Promise<Sprite | null> {
  let p = sprites.get(vtt);
  if (!p) {
    p = fetchText(vtt, setup).then((text) => {
      const cues = text ? parseSpriteVtt(text, new URL(vtt, globalThis.location?.href).href) : [];
      if (!cues.length) return null;
      return new Promise<Sprite | null>((resolve) => {
        const img = new Image();
        img.onload = () => resolve({ cues, width: img.naturalWidth, height: img.naturalHeight });
        img.onerror = () => resolve(null);
        img.src = cues[0]!.url;
      });
    });
    sprites.set(vtt, p);
    void p.then((s) => s || sprites.delete(vtt));
  }
  return p;
}

export function useSprite(vtt: string | null | undefined, setup?: XhrSetup): Sprite | null {
  const [sprite, setSprite] = useState<{ vtt: string; sprite: Sprite | null } | null>(null);
  const setupRef = useRef(setup);
  setupRef.current = setup;
  useEffect(() => {
    if (!vtt) return;
    let live = true;
    void loadSprite(vtt, setupRef.current).then((s) => live && setSprite({ vtt, sprite: s }));
    return () => {
      live = false;
    };
  }, [vtt]);
  return sprite && sprite.vtt === vtt ? sprite.sprite : null;
}

/** Why a progress report was sent. */
export type ProgressReason = "interval" | "pause" | "seek" | "ended" | "hidden" | "unload";

/** The playhead and the time actually watched, for resume points and watch history. */
export interface PlaybackProgress {
  /** Seconds. */
  time: number;
  duration: number;
  /** Seconds actually played since this source loaded; seeking adds nothing. */
  played: number;
  ended: boolean;
  reason: ProgressReason;
}

/**
 * Reports progress while a committed playback runs: every `interval` ms of
 * playback, and on pause, seek, end, page hide and unload.
 */
export function useProgressReports(
  el: HTMLVideoElement | null,
  o: { enabled: boolean; source?: string | null; interval: number; onProgress?: (p: PlaybackProgress) => void },
) {
  const cb = useRef(o.onProgress);
  cb.current = o.onProgress;
  const enabled = useRef(o.enabled);
  enabled.current = o.enabled;
  const { interval, source } = o;
  useEffect(() => {
    if (!el || !source) return;
    let played = 0;
    let sample: PlaySample | null = null;
    let last = 0;
    let reported = false;
    // The last known playhead: by unload the player may have detached the media.
    let pos = 0;
    let dur = 0;
    let ended = false;
    const observe = () => {
      if (el.currentSrc || el.readyState > 0) {
        pos = el.currentTime || 0;
        dur = Number.isFinite(el.duration) ? el.duration : dur;
        ended = el.ended;
      }
    };
    const now = (): PlaySample => ({ at: Date.now(), pos: el.currentTime || 0 });
    const tally = () => {
      const s = now();
      if (sample && !el.paused) played += playedBetween(sample, s);
      sample = s;
    };
    const report = (reason: ProgressReason) => {
      if (!enabled.current || !cb.current) return;
      reported = true;
      last = Date.now();
      cb.current({ time: pos, duration: dur, played, ended, reason });
    };
    const handlers: Record<string, () => void> = {
      playing: () => {
        observe();
        sample = now();
      },
      timeupdate: () => {
        if (el.paused || el.seeking) return;
        tally();
        observe();
        if (Date.now() - last >= interval) report("interval");
      },
      durationchange: observe,
      pause: () => {
        tally();
        observe();
        if (!el.ended) report("pause");
      },
      seeking: () => {
        sample = null;
      },
      seeked: () => {
        observe();
        sample = el.paused ? null : now();
        report("seek");
      },
      ended: () => {
        tally();
        observe();
        report("ended");
      },
    };
    let pageHide = false;
    const hidden = () => {
      if (document.visibilityState !== "hidden" && !pageHide) return;
      tally();
      observe();
      report("hidden");
    };
    const pagehide = () => {
      pageHide = true;
      hidden();
      pageHide = false;
    };
    for (const [k, h] of Object.entries(handlers)) el.addEventListener(k, h);
    document.addEventListener("visibilitychange", hidden);
    globalThis.addEventListener?.("pagehide", pagehide);
    return () => {
      for (const [k, h] of Object.entries(handlers)) el.removeEventListener(k, h);
      document.removeEventListener("visibilitychange", hidden);
      globalThis.removeEventListener?.("pagehide", pagehide);
      if (reported || played > 0) report("unload");
    };
  }, [el, source, interval]);
}

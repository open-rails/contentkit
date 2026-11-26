import { useCallback, useContext, useEffect, useMemo, useRef } from "react";
import { ProgressTracker, type OpenSource } from "../client/activity.js";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitContext, useContentKitClient } from "./context.js";
import { reporterFor } from "./discovery.js";

export interface ReadingProgressOptions {
  client?: ContentKitClient;
  enabled?: boolean;
  /** The pages of a reader; a video's length is its duration. Until known, reports carry active time only. */
  length?: number;
  /** The list the reader came from, when this page knows it; else an open from outside, or the card's. */
  source?: OpenSource;
  /** Without input for this long the reader is idle; default 60 seconds. Playback keeps them active. */
  idleAfter?: number;
  /** Active time between reports while reading; default 5 minutes. */
  every?: number;
}

export interface ReadingProgress {
  /** Attach to page `index`'s (0-based) image: the visible page with the largest area is the current page. */
  page: (index: number) => (element: HTMLElement | null) => void;
  /** The current page (1-based), for a reader that knows it, such as a paged one. */
  setPage: (page: number) => void;
  /** Attach to the playing video: its time is the point, its duration the length, and playback is active. */
  video: (element: HTMLVideoElement | null) => void;
  /** Attach to a text post's body: a single page (pass length 1), reached once shown. */
  text: (element: HTMLElement | null) => void;
  /** Reports now, with keepalive. */
  flush: () => void;
}

const INPUT = ["pointerdown", "pointermove", "keydown", "wheel", "touchstart", "touchmove", "scroll"];
const MEDIA = ["play", "playing", "pause", "ended", "waiting", "stalled", "seeked", "timeupdate", "durationchange", "loadedmetadata"];

/**
 * Reports one reading of a content item: an open when it mounts (or none
 * when a card click just reported it), then its progress: active time on a
 * visible tab, the furthest and current page or second and the length, on
 * leave and every five active minutes. Keep it in the content page, above
 * renderer switches. Reading state on the server follows within about 15
 * minutes.
 */
export function useReadingProgress(contentId: string | null | undefined, options: ReadingProgressOptions = {}): ReadingProgress {
  const client = useContentKitClient(options.client);
  const viewer = useContext(ContentKitContext)?.viewer;
  const enabled = !!contentId && options.enabled !== false && viewer !== undefined;
  const length = useRef(options.length);
  length.current = options.length;
  const source = useRef(options.source);
  source.current = options.source;

  const state = useMemo(() => {
    const pages = new Map<number, { element: HTMLElement | null; ref: (e: HTMLElement | null) => void }>();
    const areas = new Map<Element, number>();
    let observer: IntersectionObserver | undefined;
    let tracker: ProgressTracker | undefined;
    let video: HTMLVideoElement | null = null;
    let waiting = false;
    let explicit: number | undefined;
    const current = () => {
      if (video && Number.isFinite(video.duration) && video.duration > 0) return { at: Math.floor(video.currentTime), length: Math.floor(video.duration) };
      if (explicit !== undefined) return { at: explicit, length: length.current };
      let best = -1, area = 0;
      for (const [index, page] of pages) {
        const a = page.element ? areas.get(page.element) ?? 0 : 0;
        if (a > area) { area = a; best = index; }
      }
      return { at: best >= 0 ? best + 1 : 0, length: length.current };
    };
    const sample = () => {
      if (!tracker) return;
      const point = current();
      if (point.at > 0) tracker.point(point.at, point.length);
      else if (point.length) tracker.point(0, point.length);
    };
    const observe = (element: Element) => observer?.observe(element);
    const page = (index: number) => {
      let p = pages.get(index);
      if (!p) {
        const binding = { element: null as HTMLElement | null, ref: (element: HTMLElement | null) => {
          if (binding.element) { observer?.unobserve(binding.element); areas.delete(binding.element); }
          binding.element = element;
          if (element) observe(element);
        } };
        pages.set(index, (p = binding));
      }
      return p.ref;
    };
    const media = () => {
      if (!video || !tracker) return;
      if (video.paused || video.ended) waiting = false;
      tracker.setPlaying(!video.paused && !video.ended && !waiting);
      sample();
    };
    const mediaEvent = (event: Event) => {
      if (event.type === "waiting" || event.type === "stalled") waiting = true;
      else if (event.type === "playing" || event.type === "seeked") waiting = false;
      media();
    };
    const bindVideo = (element: HTMLVideoElement | null) => {
      for (const event of MEDIA) video?.removeEventListener(event, mediaEvent);
      video = element;
      waiting = false;
      for (const event of MEDIA) video?.addEventListener(event, mediaEvent);
      media();
    };
    return {
      page, bindVideo, sample,
      setPage: (n: number) => { explicit = Number.isFinite(n) && n > 0 ? Math.floor(n) : undefined; tracker?.activity(); sample(); },
      start: (id: string) => {
        tracker = new ProgressTracker({ contentId: id, idleAfter: options.idleAfter, every: options.every,
          report: (p, keepalive) => reporterFor(client).progress(id, p, keepalive) });
        if (typeof IntersectionObserver !== "undefined") {
          observer = new IntersectionObserver((entries) => {
            for (const e of entries) areas.set(e.target, e.isIntersecting ? e.intersectionRect.width * e.intersectionRect.height : 0);
            sample();
          }, { threshold: [0, 0.25, 0.5, 0.75, 1] });
          for (const p of pages.values()) if (p.element) observe(p.element);
        }
        tracker.setVisible(typeof document === "undefined" || document.visibilityState !== "hidden");
        media();
        sample();
        return tracker;
      },
      stop: () => {
        observer?.disconnect();
        observer = undefined;
        sample();
        tracker?.stop();
        tracker = undefined;
      },
      tracker: () => tracker,
    };
    // A new reader (sign-in or sign-out) starts a new reading.
  }, [client, contentId, viewer, options.idleAfter, options.every]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!enabled || !contentId) return;
    reporterFor(client).open(contentId, source.current);
    const tracker = state.start(contentId);
    const activity = () => { tracker.activity(); state.sample(); };
    const visibility = () => tracker.setVisible(document.visibilityState !== "hidden");
    const leave = () => { state.sample(); tracker.leave(); };
    const timer = setInterval(() => { state.sample(); tracker.tick(); }, 1000);
    for (const event of INPUT) window.addEventListener(event, activity, { passive: true, capture: true });
    document.addEventListener("visibilitychange", visibility);
    window.addEventListener("pagehide", leave);
    return () => {
      clearInterval(timer);
      for (const event of INPUT) window.removeEventListener(event, activity, true);
      document.removeEventListener("visibilitychange", visibility);
      window.removeEventListener("pagehide", leave);
      state.stop();
    };
  }, [enabled, contentId, client, state]);

  const video = useCallback((element: HTMLVideoElement | null) => state.bindVideo(element), [state]);
  const text = useCallback((element: HTMLElement | null) => state.page(0)(element), [state]);
  const flush = useCallback(() => { state.sample(); state.tracker()?.leave(); }, [state]);
  return useMemo(() => ({ page: state.page, setPage: state.setPage, video, text, flush }), [state, video, text, flush]);
}

import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useSyncExternalStore } from "react";
import { readerSession } from "../client/beacons.js";
import type { ContentKitClient } from "../client/client.js";
import { EngagementTracker, type ReadingObservation, type VisiblePage } from "../client/engagement.js";
import { ContentKitContext, useContentKitClient } from "./context.js";

export interface ReadingEngagementOptions {
  client?: ContentKitClient;
  enabled?: boolean;
  /** Local guest history grouping only; the server infers kind from content. */
  kind?: string;
  /** Genuine input renews attention for this long; default 60 seconds. */
  idleAfter?: number;
  /** Snapshot interval, at most 120 seconds. Page changes never send requests. */
  checkpointInterval?: number;
}

export interface ReadingEngagement {
  /** Attach to the actual loaded image, not its placeholder or thumbnail. */
  page: (index: number, resume?: string) => (element: HTMLImageElement | null) => void;
  /** Attach to the playing video. Seeks do not count as watched intervals. */
  video: (element: HTMLVideoElement | null) => void;
  /** Attach to a text post's body: one unit covered by its visible share. */
  text: (element: HTMLElement | null) => void;
  /** Stable source-hash/anchor selector to restore, independent of page index. */
  setResume: (resume: string | undefined) => void;
  flush: () => Promise<void>;
}

interface PageBinding {
  element: HTMLElement | null;
  resume?: string;
  ref: (element: HTMLElement | null) => void;
}

function visibleRange(element: HTMLElement): { from: number; to: number; area: number } | undefined {
  if (!element.isConnected) return;
  const style = getComputedStyle(element);
  if (style.display === "none" || style.visibility === "hidden" || (style.opacity !== "" && Number(style.opacity) === 0)) return;
  const box = element.getBoundingClientRect();
  if (box.width <= 0 || box.height <= 0) return;
  let top = box.top, left = box.left, width = box.width, height = box.height;
  if (element instanceof HTMLImageElement && style.objectFit === "contain") {
    const scale = Math.min(width / element.naturalWidth, height / element.naturalHeight);
    const imageWidth = element.naturalWidth * scale, imageHeight = element.naturalHeight * scale;
    const [x = "50%", y = "50%"] = style.objectPosition.split(" ");
    const offset = (position: string, space: number) => position.endsWith("%") ? space * Number.parseFloat(position) / 100 : space / 2;
    left += offset(x, width - imageWidth); top += offset(y, height - imageHeight);
    width = imageWidth; height = imageHeight;
  }
  const viewport = element.closest("[data-reader-viewport]")?.getBoundingClientRect();
  const visibleTop = Math.max(top, 0, viewport?.top ?? 0);
  const visibleBottom = Math.min(top + height, window.innerHeight, viewport?.bottom ?? window.innerHeight);
  const visibleLeft = Math.max(left, 0, viewport?.left ?? 0);
  const visibleRight = Math.min(left + width, window.innerWidth, viewport?.right ?? window.innerWidth);
  if (visibleBottom <= visibleTop || visibleRight <= visibleLeft) return;
  return { from: Math.max(0, Math.floor((visibleTop - top) / height * 10_000)),
    to: Math.min(10_000, Math.ceil((visibleBottom - top) / height * 10_000)),
    area: (visibleBottom - visibleTop) * (visibleRight - visibleLeft) };
}

/**
 * Keep this hook in the content page, above horizontal/vertical renderer switches.
 * It records loaded, visible attention, with one visit and bounded access receipts;
 * preloads and route opens alone produce no engagement or network writes.
 */
export function useReadingEngagement(contentId: string | null | undefined, versionId?: string, options: ReadingEngagementOptions = {}): ReadingEngagement {
  const client = useContentKitClient(options.client);
  const context = useContext(ContentKitContext);
  const identity = useSyncExternalStore(client.preferences.subscribeIdentity, client.preferences.identity, client.preferences.identity);
  const owner = context ? context.viewer : identity;
  const explicitGuest = context?.viewer === null;
  const permitted = !!contentId && options.enabled !== false && owner !== undefined;
  const current = useRef({ owner, permitted });
  current.current = { owner, permitted };
  const kind = useRef(options.kind);
  kind.current = options.kind;
  const language = useRef(context?.language);
  language.current = context?.language;

  const state = useMemo(() => {
    const pages = new Map<number, PageBinding>();
    const intersections = new WeakMap<Element, boolean>();
    let observer: IntersectionObserver | undefined;
    let active = false, hidden = false, buffering = false, video: HTMLVideoElement | null = null, resume: string | undefined;
    // A visit belongs to its owner; disabling tracking (e.g. closing the reader) ends it, an owner change discards it.
    const sameOwner = () => current.current.owner === owner && client.preferences.identity() === owner;
    const owns = () => permitted && current.current.permitted && sameOwner();
    let session: string | undefined;
    const getSession = () => owner === null ? (session ??= readerSession()) : undefined;
    const tracker = new EngagementTracker({
      contentId: contentId ?? "", versionId, idleAfter: options.idleAfter, checkpointInterval: options.checkpointInterval,
      begin: async () => {
        if (!owns()) return null;
        const result = await client.discovery.activityContexts([{ content_id: contentId!, version_id: versionId }], getSession(), language.current);
        if (!owns() || !result.enabled) return null;
        return result.contexts.find((receipt) => receipt.content_id === contentId && (!versionId || receipt.version_id === versionId)) ?? null;
      },
      submit: async (snapshot, submitOptions) => {
        if (!sameOwner()) return;
        await client.discovery.engagement([snapshot], { session: getSession(), ...submitOptions });
      },
      onSnapshot: (snapshot) => {
        if (sameOwner() && owner === null && explicitGuest) client.guestHistory.record({ ...snapshot, ...(kind.current ? { kind: kind.current } : {}) });
      },
    });
    const sample = () => {
      if (!active || !owns()) return;
      const visible: VisiblePage[] = [];
      let selected: { area: number; resume?: string } | undefined;
      for (const [index, binding] of pages) {
        const image = binding.element;
        // An image counts once loaded; a text body as soon as it shows.
        if (!image || image instanceof HTMLImageElement && (!image.complete || image.naturalWidth <= 0) || intersections.get(image) === false) continue;
        const range = visibleRange(image);
        if (!range) continue;
        visible.push({ index, ranges: [{ from: range.from, to: range.to }] });
        if (!selected || range.area > selected.area) selected = { area: range.area, resume: binding.resume };
      }
      const observation: ReadingObservation = { pages: visible, resume: resume ?? selected?.resume };
      if (video && video.readyState >= 2 && visibleRange(video)) observation.video = {
        positionMs: Math.max(0, Math.round(video.currentTime * 1000)), playing: !buffering && !video.paused && !video.ended && !video.seeking,
        seeking: video.seeking,
      };
      if (observation.video) observation.resume = String(video!.currentTime);
      tracker.observe(observation);
      tracker.attention(!hidden && document.visibilityState !== "hidden" && document.hasFocus());
    };
    const page = (index: number, selector?: string) => {
      let binding = pages.get(index);
      if (!binding) {
        binding = { element: null, resume: selector, ref: (element) => {
          if (binding!.element) {
            binding!.element.removeEventListener("load", sample);
            observer?.unobserve(binding!.element);
          }
          binding!.element = element;
          if (element) observer?.observe(element);
          element?.addEventListener("load", sample);
          sample();
        } };
        pages.set(index, binding);
      }
      binding.resume = selector;
      return binding.ref;
    };
    const mediaEvents = ["play", "playing", "canplay", "pause", "seeking", "seeked", "ended", "loadeddata", "ratechange", "waiting", "stalled"];
    const mediaEvent = (event: Event) => {
      if (event.type === "waiting" || event.type === "stalled") buffering = true;
      else if (["playing", "canplay", "seeked", "loadeddata"].includes(event.type)) buffering = false;
      sample();
    };
    const bindVideo = (element: HTMLVideoElement | null) => {
      for (const event of mediaEvents) video?.removeEventListener(event, mediaEvent);
      if (video) observer?.unobserve(video);
      video = element; buffering = false;
      if (video) observer?.observe(video);
      for (const event of mediaEvents) video?.addEventListener(event, mediaEvent);
      sample();
    };
    const flush = () => { sample(); return sameOwner() ? tracker.flush() : Promise.resolve(); };
    return {
      tracker, sample, owns, sameOwner, page, bindVideo, flush,
      setResume: (value: string | undefined) => { resume = value; sample(); },
      start: () => {
        active = true;
        if (!observer && typeof IntersectionObserver !== "undefined") {
          observer = new IntersectionObserver((entries) => {
            for (const entry of entries) intersections.set(entry.target, entry.isIntersecting);
            sample();
          });
          for (const binding of pages.values()) if (binding.element) observer.observe(binding.element);
          if (video) observer.observe(video);
        }
        sample();
      },
      stop: () => { active = false; },
      isActive: () => active,
      hide: () => { sample(); hidden = true; tracker.attention(false); void tracker.flush(true); },
      show: () => { hidden = false; tracker.activity(); sample(); },
      activity: () => { sample(); tracker.activity(); },
      cleanup: () => {
        observer?.disconnect();
        for (const binding of pages.values()) binding.element?.removeEventListener("load", sample);
        for (const event of mediaEvents) video?.removeEventListener(event, mediaEvent);
      },
    };
    // Language and renderer mode do not start new visits. Owner changes do.
  }, [client, contentId, versionId, owner, explicitGuest, permitted, options.idleAfter, options.checkpointInterval]);

  useLayoutEffect(() => client.preferences.subscribeIdentity(() => {
    if (!state.sameOwner()) state.tracker.discard();
  }), [client, state]);

  useEffect(() => {
    if (!permitted || !state.owns()) return;
    state.start();
    const timer = setInterval(state.sample, 250);
    const inputEvents = ["pointerdown", "pointermove", "keydown", "wheel", "touchstart", "touchmove"];
    for (const event of inputEvents) window.addEventListener(event, state.activity, { passive: true, capture: true });
    const visibility = () => { if (document.visibilityState === "hidden") state.hide(); else state.show(); };
    const blur = () => { state.sample(); state.tracker.attention(false); };
    window.addEventListener("scroll", state.sample, true);
    window.addEventListener("resize", state.sample);
    window.addEventListener("focus", state.show);
    window.addEventListener("blur", blur);
    window.addEventListener("pagehide", state.hide);
    window.addEventListener("pageshow", state.show);
    window.addEventListener("online", state.flush);
    document.addEventListener("visibilitychange", visibility);
    return () => {
      state.stop(); clearInterval(timer);
      for (const event of inputEvents) window.removeEventListener(event, state.activity, true);
      window.removeEventListener("scroll", state.sample, true);
      window.removeEventListener("resize", state.sample);
      window.removeEventListener("focus", state.show);
      window.removeEventListener("blur", blur);
      window.removeEventListener("pagehide", state.hide);
      window.removeEventListener("pageshow", state.show);
      window.removeEventListener("online", state.flush);
      document.removeEventListener("visibilitychange", visibility);
      // StrictMode's immediate setup replay must keep the same visit alive.
      queueMicrotask(() => {
        if (state.isActive()) return;
        state.cleanup();
        if (!state.sameOwner()) { state.tracker.discard(); return; }
        state.tracker.attention(false);
        void state.tracker.flush(true).finally(() => state.tracker.discard());
      });
    };
  }, [permitted, state]);

  const video = useCallback((element: HTMLVideoElement | null) => state.bindVideo(element), [state]);
  const text = useCallback((element: HTMLElement | null) => state.page(0)(element), [state]);
  return useMemo(() => ({ page: state.page, video, text, setResume: state.setResume, flush: state.flush }), [state, video, text]);
}

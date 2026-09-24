import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MouseEvent, type PointerEvent } from "react";
import type { GalleryView } from "./gallery.js";
import type { CapLevelController, Level } from "hls.js";
import {
  NETWORK_FAILURES_BEFORE_ERROR,
  abrHlsConfig,
  capRung,
  classifyHlsError,
  classifyMediaError,
  hlsConfig,
  initialEstimate,
  startRung,
  statusKind,
  type AbrPolicy,
  type ConnectionHint,
  type PlaybackError,
} from "./playback.js";

export const GALLERY_VIEW_KEY = "ckui.media-gallery.view";

export interface GalleryViewOptions {
  /** Controlled view. */
  view?: GalleryView;
  defaultView?: GalleryView;
  onViewChange?: (view: GalleryView) => void;
  /** localStorage key remembering the viewer's choice; null disables. */
  storageKey?: string | null;
}

function stored(key: string | null): GalleryView | undefined {
  if (!key) return undefined;
  try {
    const v = globalThis.localStorage?.getItem(key);
    return v === "grid" || v === "carousel" ? v : undefined;
  } catch {
    return undefined;
  }
}

/** Carousel or grid, controlled or not; the viewer's last choice is remembered. */
export function useGalleryView({ view, defaultView = "carousel", onViewChange, storageKey = GALLERY_VIEW_KEY }: GalleryViewOptions = {}) {
  const [own, setOwn] = useState<GalleryView>(() => stored(storageKey) ?? defaultView);
  const set = useCallback(
    (v: GalleryView) => {
      setOwn(v);
      if (storageKey)
        try {
          globalThis.localStorage?.setItem(storageKey, v);
        } catch {
          // private mode or blocked storage: the choice lasts this page
        }
      onViewChange?.(v);
    },
    [storageKey, onViewChange],
  );
  return [view ?? own, set] as const;
}

export interface CarouselOptions {
  count: number;
  index?: number;
  defaultIndex?: number;
  onIndexChange?: (index: number) => void;
}

export interface UseCarousel {
  index: number;
  go: (index: number) => void;
  next: () => void;
  prev: () => void;
  /** Pixels the track follows the finger by during a swipe. */
  offset: number;
  dragging: boolean;
  /** Spread on the swipe surface (touch-action: pan-y keeps vertical scrolling). */
  swipe: {
    onPointerDown: (e: PointerEvent<HTMLElement>) => void;
    onPointerMove: (e: PointerEvent<HTMLElement>) => void;
    onPointerUp: (e: PointerEvent<HTMLElement>) => void;
    onPointerCancel: (e: PointerEvent<HTMLElement>) => void;
    onClickCapture: (e: MouseEvent<HTMLElement>) => void;
  };
  /** ← → Home End. */
  onKeyDown: (e: KeyboardEvent<HTMLElement>) => void;
}

// A drag starting on a playing video's control bar scrubs instead of swiping.
function onControls(e: PointerEvent<HTMLElement>) {
  const t = e.target as HTMLElement;
  if (t.closest?.("[data-ckui-noswipe]")) return true;
  if (!(t instanceof HTMLVideoElement) || !t.controls) return false;
  return e.pointerType === "mouse" || e.clientY > t.getBoundingClientRect().bottom - 56;
}

/** Index, keyboard and swipe state for any carousel UI. */
export function useCarousel({ count, index: given, defaultIndex = 0, onIndexChange }: CarouselOptions): UseCarousel {
  const [own, setOwn] = useState(defaultIndex);
  const index = Math.min(Math.max(0, given ?? own), Math.max(0, count - 1));
  const go = useCallback(
    (i: number) => {
      const n = Math.min(Math.max(0, i), Math.max(0, count - 1));
      setOwn(n);
      onIndexChange?.(n);
    },
    [count, onIndexChange],
  );
  const [offset, setOffset] = useState(0);
  const [dragging, setDragging] = useState(false);
  const drag = useRef<{ id: number; x: number; y: number; t: number; width: number; axis?: "x" | "y" } | null>(null);
  const swallowClick = useRef(false);

  const end = (e: PointerEvent<HTMLElement>, cancel: boolean) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    drag.current = null;
    if (d.axis !== "x") return;
    const dx = e.clientX - d.x;
    const fast = Math.abs(dx) / Math.max(1, e.timeStamp - d.t) > 0.5;
    setDragging(false);
    setOffset(0);
    swallowClick.current = true;
    if (!cancel && (Math.abs(dx) > d.width * 0.2 || (fast && Math.abs(dx) > 30))) go(index + (dx < 0 ? 1 : -1));
  };

  return {
    index,
    go,
    next: () => go(index + 1),
    prev: () => go(index - 1),
    offset,
    dragging,
    swipe: {
      onPointerDown: (e) => {
        // A touch swipe ends without a click, so the flag must not outlive the gesture: it would eat the next tap (a nav button's).
        swallowClick.current = false;
        if (count < 2 || (e.pointerType === "mouse" && e.button !== 0) || onControls(e)) return;
        drag.current = { id: e.pointerId, x: e.clientX, y: e.clientY, t: e.timeStamp, width: e.currentTarget.clientWidth || 1 };
      },
      onPointerMove: (e) => {
        const d = drag.current;
        if (!d || d.id !== e.pointerId) return;
        const dx = e.clientX - d.x;
        const dy = e.clientY - d.y;
        if (!d.axis) {
          if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
          d.axis = Math.abs(dx) > Math.abs(dy) ? "x" : "y";
          if (d.axis === "y") {
            drag.current = null;
            return;
          }
          e.currentTarget.setPointerCapture?.(e.pointerId);
          setDragging(true);
        }
        const edge = (index === 0 && dx > 0) || (index === count - 1 && dx < 0);
        setOffset(edge ? dx / 3 : dx);
      },
      onPointerUp: (e) => end(e, false),
      onPointerCancel: (e) => end(e, true),
      onClickCapture: (e) => {
        if (!swallowClick.current) return;
        swallowClick.current = false;
        e.preventDefault();
        e.stopPropagation();
      },
    },
    onKeyDown: (e) => {
      const to = { ArrowLeft: index - 1, ArrowRight: index + 1, Home: 0, End: count - 1 }[e.key];
      if (to === undefined || (e.target as HTMLElement).closest?.("input, textarea, video, [contenteditable], [role=menu]")) return;
      e.preventDefault();
      go(to);
    },
  };
}

export type PlayerStatus = "idle" | "loading" | "playing" | "paused" | "buffering" | "ended" | "error";

export interface HlsPlayerOptions {
  /** The master playlist; nothing loads while absent. */
  src?: string | null;
  /** Adds auth to playlist and segment requests (e.g. a bearer header for the app's read API). */
  xhrSetup?: (xhr: XMLHttpRequest, url: string) => void | Promise<void>;
  /** Re-grants access after a 401/403 (refetch the read API); the player then retries once. */
  refresh?: () => unknown;
  /** Inactive pauses (a swiped-away slide). Default true. */
  active?: boolean;
  /** No playback progress and no bytes for this long is an error. Default 10 s. */
  stallTimeout?: number;
  /** Adaptive-bitrate policy; defaults start at 1080p when the connection allows. */
  abr?: AbrPolicy;
  /** localStorage key remembering the viewer's manual quality; null disables. */
  qualityKey?: string | null;
}

export const PLAYER_QUALITY_KEY = "ckui.player.quality";

/** A selectable rendition; `height` is its short side (portrait 1080×1920 is 1080). */
export interface QualityLevel {
  index: number;
  height: number;
  width: number;
  bitrate: number;
}

export interface PlayerQuality {
  /** Ascending; empty until the manifest loads, and always under native HLS (Safari), which picks for itself. */
  levels: QualityLevel[];
  /** The locked level index, or -1 for Auto. */
  selected: number;
  /** The level playing now, or -1 before the first switch. */
  current: number;
  /** Locks a level (-1: back to Auto) and remembers the choice. */
  select: (index: number) => void;
}

export interface UseHlsPlayer {
  /** Attach to the `<video>`. */
  ref: (el: HTMLVideoElement | null) => void;
  status: PlayerStatus;
  error?: PlaybackError;
  /** Playback began at least once (show native controls). */
  started: boolean;
  play: () => void;
  /** Reloads from scratch and plays. */
  retry: () => void;
  quality: PlayerQuality;
}

const HLS_TYPE = "application/vnd.apple.mpegurl";
let hinted = false;

function corsHint() {
  if (hinted || typeof console === "undefined") return;
  hinted = true;
  console.warn(
    `contentkit: video requests failed with no response (status 0). If media is served from another origin, ` +
      `media-access must allow this page: set MEDIA_ACCESS_CORS_ORIGINS to include ${globalThis.location?.origin ?? "the app origin"}.`,
  );
}

// Safari plays HLS natively and hides statuses; one playlist request tells access from absence.
function probe(url: string, setup?: HlsPlayerOptions["xhrSetup"]): Promise<number> {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    const send = () => {
      if (xhr.readyState === 0) xhr.open("GET", url, true);
      xhr.onload = () => resolve(xhr.status);
      xhr.onerror = xhr.ontimeout = () => resolve(0);
      xhr.timeout = 8000;
      xhr.send();
    };
    Promise.resolve(setup?.(xhr, url)).then(send, () => resolve(0));
  });
}

function storedQuality(key: string | null): number | "auto" | null {
  if (!key) return null;
  try {
    const v = globalThis.localStorage?.getItem(key);
    return v === "auto" ? v : v && Number(v) > 0 ? Number(v) : null;
  } catch {
    return null;
  }
}

function storeQuality(key: string | null, v: number | "auto") {
  if (!key) return;
  try {
    globalThis.localStorage?.setItem(key, String(v));
  } catch {
    // private mode or blocked storage
  }
}

// The last throughput measured on this page seeds the next player.
let measured = 0;

function connection(): ConnectionHint | null {
  const c = (globalThis.navigator as { connection?: ConnectionHint } | undefined)?.connection;
  return c ? { downlink: c.downlink, saveData: c.saveData } : null;
}

// hls.js's cap to the player's device-pixel size, with the policy's floor and ceiling.
function capController(Base: typeof CapLevelController | undefined, policy: AbrPolicy | undefined): typeof CapLevelController | undefined {
  if (!Base) return undefined;
  return class extends Base {
    override getMaxLevel(capLevelIndex: number): number {
      const levels: Level[] = (this as unknown as { hls: { levels: Level[] } | null }).hls?.levels ?? [];
      return capRung(levels.slice(0, capLevelIndex + 1), this.mediaWidth, this.mediaHeight, policy);
    }
  };
}

/**
 * HLS playback that fails fast with a reason: hls.js (lazy-loaded) or native
 * HLS, tuned retries, one grant refresh on 401/403, and a stall watchdog.
 */
export function useHlsPlayer({
  src,
  xhrSetup,
  refresh,
  active = true,
  stallTimeout = 10_000,
  abr,
  qualityKey = PLAYER_QUALITY_KEY,
}: HlsPlayerOptions): UseHlsPlayer {
  const [el, setEl] = useState<HTMLVideoElement | null>(null);
  const [status, setStatus] = useState<PlayerStatus>("idle");
  const [error, setError] = useState<PlaybackError>();
  const [started, setStarted] = useState(false);
  // Nothing loads until the first play.
  const [armed, setArmed] = useState(false);
  const [session, setSession] = useState(0);
  const want = useRef(false);
  const start = useRef<(() => void) | null>(null);
  const refreshed = useRef(false);
  const lastProgress = useRef(0);
  const [levels, setLevels] = useState<QualityLevel[]>([]);
  const [selected, setSelected] = useState(-1);
  const [current, setCurrent] = useState(-1);
  const choose = useRef<(index: number) => void>(() => {});
  const opts = useRef({ xhrSetup, refresh, abr, qualityKey });
  opts.current = { xhrSetup, refresh, abr, qualityKey };

  useEffect(() => {
    if (!el || !src || !armed) return;
    let dead = false;
    let destroy = () => {};
    let failures = 0;
    let native = false;
    setError(undefined);
    setLevels([]);
    setSelected(-1);
    setCurrent(-1);
    choose.current = () => {};
    setStatus(want.current ? "loading" : "idle");
    const fail = (e: PlaybackError) => {
      if (dead) return;
      const again = opts.current.refresh;
      if (e.kind === "access" && again && !refreshed.current) {
        refreshed.current = true;
        dead = true;
        destroy();
        setStatus("loading");
        Promise.resolve()
          .then(again)
          .catch(() => {})
          .finally(() => setSession((s) => s + 1));
        return;
      }
      if (e.kind === "network") corsHint();
      dead = true;
      destroy();
      want.current = false;
      setError(e);
      setStatus("error");
    };
    const onMediaError = () => native && fail(classifyMediaError(el.error));
    el.addEventListener("error", onMediaError);
    import("hls.js").then(
      ({ default: Hls }) => {
        if (dead) return;
        if (Hls.isSupported()) {
          let loading = false;
          const conn = connection();
          const policy = conn?.saveData ? { ...opts.current.abr, preferHighRes: false } : opts.current.abr;
          const estimate = initialEstimate(policy, conn, measured);
          const hls = new Hls({
            ...hlsConfig,
            ...abrHlsConfig(policy, estimate),
            capLevelController: capController(Hls.DefaultConfig.capLevelController, policy),
            autoStartLoad: false,
            xhrSetup: (xhr, url) => opts.current.xhrSetup?.(xhr, url),
          });
          destroy = () => hls.destroy();
          hls.on(Hls.Events.ERROR, (_, d) => {
            const e = classifyHlsError(d);
            if (d.fatal || e.kind === "access" || e.kind === "not_found" || e.kind === "rate_limited") fail(e);
            else if (e.kind === "network" && ++failures >= NETWORK_FAILURES_BEFORE_ERROR) fail(e);
          });
          // A manual choice before loading applies at start.
          let locked = -1;
          choose.current = (i) => {
            locked = i;
            setSelected(i);
            if (!loading) return;
            // Flushes the buffer ahead so the choice shows within a segment.
            hls.nextLevel = i;
          };
          hls.on(Hls.Events.LEVEL_SWITCHED, (_, d) => setCurrent(d.level));
          hls.on(Hls.Events.FRAG_LOADED, () => {
            failures = 0;
            lastProgress.current = Date.now();
            measured = hls.bandwidthEstimate;
          });
          let parsed = false;
          start.current = () => {
            if (!parsed) return;
            if (!loading) {
              loading = true;
              const box = el.getBoundingClientRect();
              const dpr = globalThis.devicePixelRatio || 1;
              const cap = capRung(hls.levels, box.width * dpr, box.height * dpr, policy);
              hls.startLevel = locked >= 0 ? locked : startRung(hls.levels, estimate, cap, conn);
              if (locked >= 0) hls.loadLevel = locked;
              hls.startLoad(-1);
            }
            el.play().catch(() => {});
          };
          hls.on(Hls.Events.MANIFEST_PARSED, () => {
            parsed = true;
            const max = policy?.maxHeight ?? Infinity;
            const list = hls.levels
              .map((l, index) => ({ index, height: Math.min(l.width, l.height) || l.height, width: l.width, bitrate: l.bitrate }))
              .filter((l) => l.height > 0 && l.height <= max);
            setLevels(list);
            // A remembered rung this video lacks (e.g. 2160p not yet encoded) plays Auto.
            const saved = storedQuality(opts.current.qualityKey);
            const match = typeof saved === "number" ? list.find((l) => l.height === saved) : undefined;
            if (match) choose.current(match.index);
            if (want.current) start.current?.();
          });
          hls.loadSource(src);
          hls.attachMedia(el);
          return;
        }
        if (!el.canPlayType(HLS_TYPE)) return fail({ kind: "unsupported", code: "no-hls" });
        void probe(src, opts.current.xhrSetup).then((code) => {
          if (dead) return;
          if (code < 200 || code >= 400) return fail({ kind: statusKind(code), code: `probe/${code}`, status: code });
          native = true;
          el.src = src;
          destroy = () => {
            el.removeAttribute("src");
            el.load();
          };
          start.current = () => void el.play().catch(() => {});
          if (want.current) start.current();
        });
      },
      () => fail({ kind: "unsupported", code: "hls.js" }),
    );
    return () => {
      dead = true;
      start.current = null;
      el.removeEventListener("error", onMediaError);
      destroy();
    };
  }, [el, src, armed, session]);

  // Media element state.
  useEffect(() => {
    if (!el) return;
    const set = (s: PlayerStatus) => () => setStatus((cur) => (cur === "error" ? cur : s));
    const handlers: Record<string, () => void> = {
      playing: () => {
        setStarted(true);
        refreshed.current = false;
        lastProgress.current = Date.now();
        set("playing")();
      },
      pause: () => {
        if (!el.ended) set("paused")();
      },
      waiting: () => want.current && set("buffering")(),
      ended: set("ended"),
      progress: () => (lastProgress.current = Date.now()),
      play: () => {
        want.current = true;
        lastProgress.current = Date.now();
      },
    };
    for (const [k, h] of Object.entries(handlers)) el.addEventListener(k, h);
    return () => {
      for (const [k, h] of Object.entries(handlers)) el.removeEventListener(k, h);
    };
  }, [el]);

  // Only the visible player plays.
  useEffect(() => {
    if (!active && el && !el.paused) el.pause();
    if (!active) want.current = false;
  }, [active, el]);

  // Stall watchdog: wanting playback, yet neither time nor bytes move.
  const watching = status === "loading" || status === "buffering" || status === "playing";
  useEffect(() => {
    if (!watching || !el) return;
    let time = el.currentTime;
    lastProgress.current = Math.max(lastProgress.current, Date.now());
    const id = setInterval(() => {
      if (!want.current || el.paused) return;
      if (el.currentTime !== time) {
        time = el.currentTime;
        lastProgress.current = Date.now();
      } else if (Date.now() - lastProgress.current > stallTimeout) {
        el.pause();
        want.current = false;
        setError({ kind: "stalled", code: "stall" });
        setStatus("error");
      }
    }, 1000);
    return () => clearInterval(id);
  }, [watching, el, stallTimeout]);

  const retry = useCallback(() => {
    want.current = true;
    refreshed.current = false;
    lastProgress.current = Date.now();
    setError(undefined);
    setStatus("loading");
    setArmed(true);
    setSession((s) => s + 1);
  }, []);

  const play = useCallback(() => {
    if (error) return retry();
    want.current = true;
    lastProgress.current = Date.now();
    setStatus("loading");
    setArmed(true);
    start.current?.();
  }, [error, retry]);

  const levelsRef = useRef(levels);
  levelsRef.current = levels;
  const select = useCallback((index: number) => {
    choose.current(index);
    const h = levelsRef.current.find((l) => l.index === index)?.height;
    storeQuality(opts.current.qualityKey, index === -1 || !h ? "auto" : h);
  }, []);

  return { ref: setEl, status, error, started, play, retry, quality: { levels, selected, current, select } };
}

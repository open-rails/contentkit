import { createContext, useContext, useEffect, useRef, useState } from "react";

/** The host's default for inline previews (UploadUiProvider `inlinePreview`). */
export const InlinePreviewContext = createContext(true);

/** Hover this long (ms) before a preview starts. */
export const HOVER_DELAY = 500;
/** A touch-device preview needs this share of the element (or of the viewport) in view. */
const MIN_VISIBLE = 0.5;

interface Entry {
  start: () => void;
  stop: () => void;
}

// One preview at a time, page-wide; none while a video plays for real.
let current: Entry | null = null;
const visible = new Map<Entry, number>();
const playing = new Set<Entry>();
let pickTimer: ReturnType<typeof setTimeout> | undefined;

function activate(e: Entry) {
  if (current === e || playing.size) return;
  current?.stop();
  current = e;
  e.start();
}

function deactivate(e: Entry) {
  if (current !== e) return;
  current = null;
  e.stop();
}

// Touch devices: the most visible candidate plays.
function schedulePick() {
  clearTimeout(pickTimer);
  pickTimer = setTimeout(() => {
    let best: Entry | null = null;
    let score = MIN_VISIBLE;
    for (const [e, s] of visible) if (s >= score) [best, score] = [e, s];
    if (best) activate(best);
    else if (current) deactivate(current);
  }, 150);
}

function setPlaying(e: Entry, on: boolean) {
  if (on) {
    playing.add(e);
    if (current && current !== e) deactivate(current);
    if (current === e) current = null; // committed: no longer a preview
  } else if (playing.delete(e) && visible.size) schedulePick();
}

const query = (q: string) => typeof matchMedia === "function" && matchMedia(q).matches;

/** The user's reduced-motion preference, live. */
export function useReducedMotion(): boolean {
  const q = "(prefers-reduced-motion: reduce)";
  const [reduced, setReduced] = useState(() => query(q));
  useEffect(() => {
    if (typeof matchMedia !== "function") return;
    const m = matchMedia(q);
    const on = () => setReduced(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return reduced;
}

export interface InlinePreviewOptions {
  /** The component's own setting; default the provider's. */
  setting?: boolean;
  /** Whether this element can preview now (a playable video, not yet played). */
  available: boolean;
  /** Hovered (fine pointers) or watched for visibility (touch). */
  target: HTMLElement | null;
  /** Plays for real: stops any preview and holds new ones off. */
  playing?: boolean;
  start: () => void;
  stop: () => void;
}

/**
 * Drives one element's inline preview: on hover after HOVER_DELAY with a fine
 * pointer, else while it is the most visible candidate. Off with reduced
 * motion, Save-Data or the host setting.
 */
export function useInlinePreview(o: InlinePreviewOptions): void {
  const host = useContext(InlinePreviewContext);
  const reduced = useReducedMotion();
  const fns = useRef(o);
  fns.current = o;
  const entry = useRef<Entry>(null!);
  entry.current ??= { start: () => fns.current.start(), stop: () => fns.current.stop() };
  const saveData = !!(globalThis.navigator as { connection?: { saveData?: boolean } } | undefined)?.connection?.saveData;
  const enabled = (o.setting ?? host) && o.available && !reduced && !saveData;
  const { target } = o;

  useEffect(() => {
    const e = entry.current;
    if (!enabled || !target) return;
    if (query("(hover: hover) and (pointer: fine)")) {
      let timer: ReturnType<typeof setTimeout> | undefined;
      const enter = (ev: PointerEvent) => {
        if (ev.pointerType && ev.pointerType !== "mouse") return;
        clearTimeout(timer);
        timer = setTimeout(() => activate(e), HOVER_DELAY);
      };
      const leave = () => {
        clearTimeout(timer);
        deactivate(e);
      };
      target.addEventListener("pointerenter", enter);
      target.addEventListener("pointerleave", leave);
      return () => {
        clearTimeout(timer);
        target.removeEventListener("pointerenter", enter);
        target.removeEventListener("pointerleave", leave);
        deactivate(e);
      };
    }
    if (typeof IntersectionObserver !== "function") return;
    const io = new IntersectionObserver(
      ([x]) => {
        if (!x) return;
        const view = x.rootBounds?.height || globalThis.innerHeight || 1;
        const score = x.isIntersecting ? Math.max(x.intersectionRatio, x.intersectionRect.height / view) : 0;
        if (score > 0) visible.set(e, score);
        else visible.delete(e);
        schedulePick();
      },
      { threshold: [0, 0.25, 0.5, 0.75, 1] },
    );
    io.observe(target);
    return () => {
      io.disconnect();
      visible.delete(e);
      deactivate(e);
      schedulePick();
    };
  }, [enabled, target]);

  const on = !!o.playing;
  useEffect(() => {
    const e = entry.current;
    setPlaying(e, on);
    return () => setPlaying(e, false);
  }, [on]);
}

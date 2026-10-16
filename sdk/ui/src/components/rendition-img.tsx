import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ComponentProps, type CSSProperties, type ReactNode } from "react";
import { DEFAULT_DENSITY, densityFor, pickRendition, sortRenditions, type DensityRange, type Rendition } from "../client/rendition.js";
import { useReducedMotion } from "../react/inline-preview.js";

export const DensityContext = createContext<DensityRange>(DEFAULT_DENSITY);

const useIsoLayoutEffect = typeof window === "undefined" ? useEffect : useLayoutEffect;

export interface UseRenditionOptions {
  /** Density range; default the provider's (1–3×). */
  density?: DensityRange;
}

/**
 * Picks a rendition for the element `ref` (a callback ref) is attached to: the narrowest at
 * least its rendered width × density. It re-picks on resize (fullscreen,
 * window changes), never steps down, and keeps the shown rendition until a
 * wider one has loaded, so nothing flickers.
 */
export function useRendition<T extends Rendition, E extends HTMLElement = HTMLImageElement>(outputs: readonly T[] | null | undefined, o: UseRenditionOptions = {}) {
  const provided = useContext(DensityContext);
  const range = o.density ?? provided;
  const [el, ref] = useState<E | null>(null);
  // -1 until measured; a hidden element measures 0 and gets the narrowest.
  const [width, setWidth] = useState(-1);
  useIsoLayoutEffect(() => {
    if (!el) return;
    let live = true;
    const measure = () => { if (live) setWidth((w) => Math.max(w, el.getBoundingClientRect().width)); };
    measure();
    const ro = typeof ResizeObserver === "function" ? new ResizeObserver(measure) : undefined;
    ro?.observe(el);
    return () => { live = false; ro?.disconnect(); };
  }, [el]);
  const sorted = useMemo(() => sortRenditions(outputs), [outputs]);
  const want = width >= 0 ? pickRendition(sorted, width, densityFor(range)) : undefined;
  const [shownURL, setShownURL] = useState<string | undefined>(undefined);
  const loaded = useRef<string | undefined>(undefined);
  const failedUpgrade = useRef<string | null>(null);
  const current = sorted.find((r) => r.url === shownURL);
  const wantURL = want?.url;
  const wantWidth = want?.w ?? 0;
  const currentURL = current?.url;
  const currentWidth = current?.w ?? 0;
  useEffect(() => {
    if (!wantURL || wantURL === currentURL) return;
    // First pick, a new set, or before anything loaded: show it now.
    if (!currentURL || loaded.current !== currentURL) {
      setShownURL(wantURL);
      return;
    }
    if (wantWidth <= currentWidth || failedUpgrade.current === wantURL) return;
    let live = true;
    const img = new Image();
    const show = () => { if (live) setShownURL(wantURL); };
    img.onload = show;
    img.onerror = () => { if (live) failedUpgrade.current = wantURL; };
    img.src = wantURL;
    if (img.complete && img.naturalWidth > 0) show();
    return () => {
      live = false;
      img.onload = null;
      img.onerror = null;
    };
  }, [wantURL, wantWidth, currentURL, currentWidth]);
  const onLoad = useCallback(() => {
    loaded.current = currentURL ?? wantURL;
  }, [currentURL, wantURL]);
  return { ref, rendition: current ?? want, onLoad };
}

export interface RenditionImgProps extends Omit<ComponentProps<"img">, "src" | "srcSet" | "sizes" | "width" | "height"> {
  outputs: readonly Rendition[] | null | undefined;
  density?: DensityRange;
  /** Rendered instead when there are no renditions or the image fails to load (a 404). */
  fallback?: ReactNode;
  /** A pulsing muted fill until the image has loaded. */
  skeleton?: boolean;
  /** object-fit of the image in its box. */
  fit?: "cover" | "contain";
}

const SKELETON: CSSProperties = { backgroundColor: "var(--ckui-muted, rgb(127 127 127 / 0.15))" };
const PULSE = "ckui-pulse 2s cubic-bezier(0.4, 0, 0.6, 1) infinite";

/**
 * An `<img>` showing the rendition its rendered size × density needs (see
 * useRendition), with a fallback for none or a failed load and an optional
 * loading skeleton.
 */
export function RenditionImg({ outputs, density, onLoad, onError, alt = "", fallback, skeleton, fit, style, ...img }: RenditionImgProps) {
  const { ref, rendition, onLoad: loaded } = useRendition(outputs, { density });
  const sorted = useMemo(() => sortRenditions(outputs), [outputs]);
  const first = sorted[0];
  const selected = rendition ?? first;
  const reduced = useReducedMotion();
  const [failed, setFailed] = useState<readonly string[]>([]);
  // One smaller fallback only: do not walk every variant or retry a broken
  // source on resize/rerender. A new output URL or deliberate remount retries.
  const r = selected && failed.includes(selected.url)
    ? first && first.w < selected.w && !failed.includes(first.url) ? first : undefined
    : selected;
  const [done, setDone] = useState<string | null>(null);
  const [el, setEl] = useState<HTMLImageElement | null>(null);
  const activeImage = useRef<HTMLImageElement | null>(null);
  const src = rendition ? r?.url : undefined;
  // A cached image is complete before its load event; settle it before paint.
  useIsoLayoutEffect(() => {
    if (el && src && el.getAttribute("src") === src && el.complete && el.naturalWidth > 0) {
      loaded();
      setDone(src);
    }
  }, [el, src, loaded]);
  const attach = useCallback(
    (node: HTMLImageElement | null) => {
      activeImage.current = node;
      ref(node);
      setEl(node);
    },
    [ref],
  );
  if (!r) return <>{fallback ?? null}</>;
  const loading = skeleton && done !== src;
  return (
    <img
      {...img}
      key={src}
      ref={attach}
      alt={alt}
      src={src}
      width={r.w}
      height={r.h || undefined}
      decoding="async"
      data-rendition={rendition ? r.w : undefined}
      data-loading={loading ? "" : undefined}
      style={{ ...(fit ? { objectFit: fit } : {}), ...(loading ? { ...SKELETON, animation: reduced ? undefined : PULSE } : {}), ...style }}
      onLoad={(e) => {
        if (activeImage.current !== e.currentTarget) return;
        loaded();
        if (src) setDone(src);
        onLoad?.(e);
      }}
      onError={(e) => {
        if (activeImage.current !== e.currentTarget) return;
        if (src) setFailed((previous) => [...previous.filter((url) => sorted.some((r) => r.url === url)), src]);
        onError?.(e);
      }}
    />
  );
}

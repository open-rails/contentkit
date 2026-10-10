import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ComponentProps, type CSSProperties, type ReactNode } from "react";
import { DEFAULT_DENSITY, densityFor, pickRendition, sortRenditions, type DensityRange, type Rendition } from "../client/rendition.js";
import { useReducedMotion } from "../react/inline-preview.js";

export const DensityContext = createContext<DensityRange>(DEFAULT_DENSITY);

const useIsoLayoutEffect = typeof window === "undefined" ? useEffect : useLayoutEffect;

export interface UseRenditionOptions {
  /** Density range; default the provider's (2–3×). */
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
    const measure = () => setWidth((w) => Math.max(w, el.getBoundingClientRect().width));
    measure();
    if (typeof ResizeObserver !== "function") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [el]);
  const sorted = useMemo(() => sortRenditions(outputs), [outputs]);
  const want = width >= 0 ? pickRendition(sorted, width, densityFor(range)) : undefined;
  const [shown, setShown] = useState<T | undefined>(undefined);
  const loaded = useRef(false);
  const current = shown && sorted.some((r) => r.url === shown.url) ? shown : undefined;
  useEffect(() => {
    if (!want || want.url === current?.url) return;
    // First pick, a new set, or before anything loaded: show it now.
    if (!current || !loaded.current) {
      loaded.current = false;
      setShown(want);
      return;
    }
    if (want.w <= current.w) return;
    let live = true;
    const img = new Image();
    img.onload = () => live && setShown(want);
    img.src = want.url;
    return () => {
      live = false;
    };
  }, [want, current]);
  const onLoad = useCallback(() => {
    loaded.current = true;
  }, []);
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
  const first = sortRenditions(outputs)[0];
  const r = rendition ?? first;
  const reduced = useReducedMotion();
  const [failed, setFailed] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const [el, setEl] = useState<HTMLImageElement | null>(null);
  const src = rendition ? r?.url : undefined;
  // A cached image is complete before its load event; settle it before paint.
  useIsoLayoutEffect(() => {
    if (el && src && el.complete && el.naturalWidth > 0) setDone(src);
  }, [el, src]);
  const attach = useCallback(
    (node: HTMLImageElement | null) => {
      ref(node);
      setEl(node);
    },
    [ref],
  );
  if (!r || (first && failed === first.url)) return <>{fallback ?? null}</>;
  const loading = skeleton && done !== src;
  return (
    <img
      {...img}
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
        loaded();
        if (src) setDone(src);
        onLoad?.(e);
      }}
      onError={(e) => {
        if (first) setFailed(first.url);
        onError?.(e);
      }}
    />
  );
}

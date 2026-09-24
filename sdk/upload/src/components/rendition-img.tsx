import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ComponentProps } from "react";
import { DEFAULT_DENSITY, densityFor, pickRendition, sortRenditions, type DensityRange, type Rendition } from "../rendition.js";

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
}

/** An `<img>` showing the rendition its rendered size × density needs (see useRendition). */
export function RenditionImg({ outputs, density, onLoad, alt = "", ...img }: RenditionImgProps) {
  const { ref, rendition, onLoad: loaded } = useRendition(outputs, { density });
  const first = sortRenditions(outputs)[0];
  const r = rendition ?? first;
  if (!r) return null;
  return (
    <img
      {...img}
      ref={ref}
      alt={alt}
      src={rendition ? r.url : undefined}
      width={r.w}
      height={r.h}
      decoding="async"
      data-rendition={rendition ? r.w : undefined}
      onLoad={(e) => {
        loaded();
        onLoad?.(e);
      }}
    />
  );
}

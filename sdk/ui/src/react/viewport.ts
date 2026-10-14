import { useEffect, useState } from "react";

export interface NearViewportOptions {
  /** How near (IntersectionObserver rootMargin) counts as near. Default "800px 0px". */
  margin?: string;
  /**
   * Not near again once beyond this margin (wider than margin, so scrolling
   * back and forth over the edge does not thrash). Default: once near, always near.
   */
  leaveMargin?: string;
  /** Near from the start (content above the fold). */
  eager?: boolean;
  /** The scroll container; default the viewport. */
  root?: Element | null;
}

/**
 * Whether the element (attach the returned callback ref) is near the
 * viewport: mount media as it approaches, unmount it far away. Without
 * IntersectionObserver everything is near.
 */
export function useNearViewport(o: NearViewportOptions = {}): [ref: (el: Element | null) => void, near: boolean] {
  const { margin = "800px 0px", leaveMargin, eager = false, root } = o;
  const [el, ref] = useState<Element | null>(null);
  const [near, setNear] = useState(eager || typeof IntersectionObserver !== "function");
  const done = near && !leaveMargin;
  useEffect(() => {
    if (!el || done || typeof IntersectionObserver !== "function") return;
    const enter = new IntersectionObserver((es) => es.some((e) => e.isIntersecting) && setNear(true), { rootMargin: margin, root: root ?? null });
    enter.observe(el);
    const leave = leaveMargin
      ? new IntersectionObserver((es) => es.every((e) => !e.isIntersecting) && setNear(false), { rootMargin: leaveMargin, root: root ?? null })
      : null;
    leave?.observe(el);
    return () => {
      enter.disconnect();
      leave?.disconnect();
    };
  }, [el, done, margin, leaveMargin, root]);
  return [ref, near];
}

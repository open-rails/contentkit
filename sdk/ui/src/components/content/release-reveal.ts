import { useCallback, useEffect, useLayoutEffect, useRef } from "react";
import { useReducedMotion } from "../../react/inline-preview.js";

/** A local layout transition for a cached reveal, not an image-loading policy. */
export function useReleaseReveal(identity: string) {
  const grid = useRef<HTMLOListElement>(null);
  const pending = useRef<{ height: number; positions: Map<string, DOMRect>; focus: boolean } | null>(null);
  const animations = useRef<Animation[]>([]);
  const reduced = useReducedMotion();
  const prepare = useCallback((focus = false) => {
    const element = grid.current;
    if (!element) return;
    pending.current = {
      height: element.getBoundingClientRect().height,
      positions: new Map([...element.children].map((child) => [child.getAttribute("data-release-key") ?? "", child.getBoundingClientRect()])),
      focus,
    };
  }, []);
  useLayoutEffect(() => {
    const snapshot = pending.current;
    pending.current = null;
    const element = grid.current;
    if (!snapshot || !element) return;
    animations.current.forEach((animation) => animation.cancel());
    animations.current = [];
    const children = [...element.children].filter((child): child is HTMLElement => child instanceof HTMLElement);
    const added = children.filter((child) => !snapshot.positions.has(child.dataset.releaseKey ?? ""));
    if (snapshot.focus) added[0]?.querySelector<HTMLElement>("a[href]:not([tabindex='-1']), button:not([disabled])")?.focus({ preventScroll: true });
    if (reduced || typeof element.animate !== "function") return;
    const easing = "cubic-bezier(0.22, 1, 0.36, 1)";
    let revealed = 0;
    for (const child of children) {
      const previous = snapshot.positions.get(child.dataset.releaseKey ?? "");
      if (previous) {
        const next = child.getBoundingClientRect();
        const x = previous.left - next.left;
        const y = previous.top - next.top;
        if (x || y) animations.current.push(child.animate([{ transform: `translate(${x}px, ${y}px)` }, { transform: "translate(0, 0)" }], { duration: 400, easing }));
      } else {
        animations.current.push(child.animate([{ opacity: 0, transform: "translateY(16px) scale(0.97)" }, { opacity: 1, transform: "translateY(0) scale(1)" }], { duration: 400, delay: Math.min(revealed++, 5) * 45, easing, fill: "backwards" }));
      }
    }
    const height = element.getBoundingClientRect().height;
    if (Math.abs(height - snapshot.height) > 1) animations.current.push(element.animate([{ height: `${snapshot.height}px`, overflow: "clip" }, { height: `${height}px`, overflow: "clip" }], { duration: 400, easing }));
  }, [identity, reduced]);
  useEffect(() => () => animations.current.forEach((animation) => animation.cancel()), []);
  return { grid, prepare };
}

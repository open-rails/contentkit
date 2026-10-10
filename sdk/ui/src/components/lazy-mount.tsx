import type { ComponentProps, ReactNode } from "react";
import { useNearViewport, type NearViewportOptions } from "../react/viewport.js";

export interface LazyMountProps extends NearViewportOptions, Omit<ComponentProps<"div">, "children"> {
  /** Mounted once the box nears the viewport (and, with leaveMargin, unmounted far away). */
  children?: ReactNode;
  /** Rendered while not near, e.g. a box at the content's size so the page keeps its height. */
  placeholder?: ReactNode;
}

/** A box whose children mount as it nears the viewport: feeds read and play media lazily. */
export function LazyMount({ margin, leaveMargin, eager, root, children, placeholder, ...div }: LazyMountProps) {
  const [ref, near] = useNearViewport({ margin, leaveMargin, eager, root });
  return (
    <div {...div} ref={ref} data-ckui="lazy-mount" data-near={near ? "" : undefined}>
      {near ? children : placeholder}
    </div>
  );
}

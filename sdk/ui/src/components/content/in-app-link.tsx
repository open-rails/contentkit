import { useContext, type ComponentProps } from "react";
import { ContentKitContext } from "../../react/context.js";

/** A plain link followed in-app through the provider's navigate, when the host gives one. */
export function InAppLink({ href, onClick, ...props }: ComponentProps<"a"> & { href: string }) {
  const navigate = useContext(ContentKitContext)?.navigate;
  return (
    <a
      {...props}
      href={href}
      onClick={(e) => {
        onClick?.(e);
        if (!navigate || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(href);
      }}
    />
  );
}

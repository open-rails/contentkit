import { ArrowLeft01Icon, ArrowRight01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { MouseEvent, ReactNode } from "react";
import { useMessages } from "../../i18n/context.js";
import { buttonVariants } from "#ckui/ui/button";

export interface PaginationProps {
  /** The current page, from 1. */
  page: number;
  /** Every item the pages hold; a lower bound with atLeast. */
  total: number;
  pageSize: number;
  /** total is a lower bound: more pages may follow the last known one. */
  atLeast?: boolean;
  onPageChange: (page: number) => void;
  /** Each page's link, for crawlers and opening in a new tab; clicks still call onPageChange. */
  pageHref?: (page: number) => string;
  className?: string;
}

/** The page numbers shown: first, last and two either side of the current one, with gaps. */
export function pageWindow(page: number, last: number): (number | "gap")[] {
  const shown = new Set([1, last, page - 2, page - 1, page, page + 1, page + 2].filter((n) => n >= 1 && n <= last));
  const out: (number | "gap")[] = [];
  let previous = 0;
  for (const n of [...shown].sort((a, b) => a - b)) {
    if (n - previous === 2) out.push(n - 1);
    else if (n - previous > 2) out.push("gap");
    out.push(n);
    previous = n;
  }
  return out;
}

/** Numbered page controls, for lists whose page lives in the host's URL. */
export function Pagination(p: PaginationProps) {
  const m = useMessages();
  const { t } = m;
  const size = Math.max(1, p.pageSize);
  const known = Math.max(1, Math.ceil(p.total / size));
  const page = Math.max(1, p.page);
  const last = Math.max(known, page);
  const format = (n: number) => new Intl.NumberFormat(m.language).format(n);
  // One page has nothing to choose.
  if (last <= 1 && !p.atLeast) return null;
  const control = (target: number, label: ReactNode, o: { current?: boolean; disabled?: boolean; aria?: string } = {}) => {
    const className = cn(buttonVariants({ variant: o.current ? "default" : "ghost", size: "sm" }), "min-w-8 tabular-nums", o.disabled && "pointer-events-none opacity-50");
    const go = (event: MouseEvent) => {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) return;
      event.preventDefault();
      if (!o.disabled && !o.current) p.onPageChange(target);
    };
    return p.pageHref && !o.disabled ? (
      <a href={p.pageHref(target)} className={className} aria-current={o.current ? "page" : undefined} aria-label={o.aria} onClick={go}>
        {label}
      </a>
    ) : (
      <button type="button" className={className} aria-current={o.current ? "page" : undefined} aria-label={o.aria} disabled={o.disabled} onClick={go}>
        {label}
      </button>
    );
  };
  return (
    <nav aria-label={t("listing.pages")} className={cn("flex flex-wrap items-center justify-end gap-1 text-sm", p.className)} data-ckui="pagination">
      {control(page - 1, <HugeiconsIcon icon={ArrowLeft01Icon} strokeWidth={2} />, { disabled: page <= 1, aria: t("listing.previous") })}
      {pageWindow(page, last).map((n, i) =>
        n === "gap" ? (
          <span key={`gap-${i}`} aria-hidden className="px-1 text-muted-foreground">
            …
          </span>
        ) : (
          <span key={n}>{control(n, format(n), { current: n === page, aria: t("listing.goToPage", { page: format(n) }) })}</span>
        ),
      )}
      {p.atLeast && <span aria-hidden className="px-1 text-muted-foreground">…</span>}
      {control(page + 1, <HugeiconsIcon icon={ArrowRight01Icon} strokeWidth={2} />, { disabled: page >= last && !p.atLeast, aria: t("listing.next") })}
    </nav>
  );
}

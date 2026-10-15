import { Cancel01Icon, Clock01Icon, File01Icon, Folder01Icon, Image01Icon, Search01Icon, TagsIcon, UserIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon, type IconSvgElement } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type RefObject, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Card, GroupCard } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext } from "../../react/context.js";
import { useRecentSearches, useTypeahead } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Input } from "#ckui/ui/input";
import { RenditionImg } from "../rendition-img.js";
import { labelOf, type Labels } from "../groups/parts.js";

export interface SearchBoxProps {
  /** Submits the text: the host shows its search page. */
  onSearch: (q: string) => void;
  /** Suggest contents of this kind only. */
  kind?: string;
  /** The group kinds suggested; every kind with a page by default. */
  groupKinds?: readonly string[];
  defaultValue?: string;
  placeholder?: string;
  /** A suggestion's link; default its canonical path. */
  href?: (item: Card | GroupCard) => string | undefined;
  labels?: Labels;
  /** The reader's recent searches while the box is empty. Default true. */
  recent?: boolean;
  /** Header-wide dropdown under the nearest header/banner; default stays inline. */
  presentation?: "inline" | "header";
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

type Suggestion = { type: "content"; card: Card } | { type: "group"; group: GroupCard } | { type: "recent"; query: string; searchedAt?: string };

const TYPE_ICONS: Record<string, { icon: IconSvgElement; color: string }> = {
  artist: { icon: UserIcon, color: "text-blue-500 dark:text-blue-400" },
  series: { icon: Folder01Icon, color: "text-yellow-600 dark:text-yellow-400" },
  gallery: { icon: Image01Icon, color: "text-green-600 dark:text-green-400" },
  post: { icon: File01Icon, color: "text-purple-500 dark:text-purple-400" },
  blog: { icon: File01Icon, color: "text-purple-500 dark:text-purple-400" },
  poll: { icon: File01Icon, color: "text-purple-500 dark:text-purple-400" },
  character: { icon: UserIcon, color: "text-purple-500 dark:text-purple-400" },
  tag: { icon: TagsIcon, color: "text-orange-500 dark:text-orange-400" },
};

function useHeaderTop(open: boolean, enabled: boolean, root: RefObject<HTMLDivElement | null>, input: RefObject<HTMLInputElement | null>): number {
  const [top, setTop] = useState(0);
  useLayoutEffect(() => {
    if (!open || !enabled || !root.current) return;
    const anchor = root.current.closest('header, [role="banner"]') ?? input.current ?? root.current;
    const view = root.current.ownerDocument.defaultView;
    if (!view) return;
    const measure = () => setTop(Math.max(0, Math.min(view.innerHeight, anchor.getBoundingClientRect().bottom)));
    measure();
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    observer?.observe(anchor);
    view.addEventListener("resize", measure);
    view.addEventListener("scroll", measure, true);
    view.visualViewport?.addEventListener("resize", measure);
    view.visualViewport?.addEventListener("scroll", measure);
    return () => {
      observer?.disconnect();
      view.removeEventListener("resize", measure);
      view.removeEventListener("scroll", measure, true);
      view.visualViewport?.removeEventListener("resize", measure);
      view.visualViewport?.removeEventListener("scroll", measure);
    };
  }, [open, enabled, root, input]);
  return top;
}

function SearchTypeIcon({ kind }: { kind: string }) {
  const type = TYPE_ICONS[kind];
  return <HugeiconsIcon icon={type?.icon ?? Search01Icon} strokeWidth={2} className={cn("size-4 shrink-0", type?.color ?? "text-muted-foreground")} data-search-kind={kind} aria-hidden />;
}

function SearchDate({ value, formatter }: { value?: string; formatter: Intl.DateTimeFormat }) {
  if (!value || Number.isNaN(Date.parse(value))) return null;
  return <time dateTime={value} className="shrink-0 text-xs text-muted-foreground">{formatter.format(new Date(value))}</time>;
}

/**
 * A search field suggesting contents and groups as the reader types, and
 * recent searches while it is empty; Enter searches, or opens the suggestion chosen.
 */
export function SearchBox(p: SearchBoxProps) {
  const { t, error, language } = useMessages();
  const id = useId();
  const navigate = useContext(ContentKitContext)?.navigate;
  const [text, setText] = useState(p.defaultValue ?? "");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const root = useRef<HTMLDivElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const header = p.presentation === "header";
  const top = useHeaderTop(open, header, root, input);
  const dates = useMemo(() => new Intl.DateTimeFormat(language, { dateStyle: "short" }), [language]);
  const empty = !text.trim();
  const found = useTypeahead(text, { kind: p.kind, group_kinds: p.groupKinds, client: p.client });
  const recent = useRecentSearches({ enabled: p.recent !== false && open && empty, client: p.client });
  const items = useMemo<Suggestion[]>(
    () =>
      empty
        ? recent.searches.map((r) => ({ type: "recent" as const, query: r.query, searchedAt: r.searched_at }))
        : [...found.contents.map((card) => ({ type: "content" as const, card })), ...found.groups.map((group) => ({ type: "group" as const, group }))],
    [empty, recent.searches, found.contents, found.groups],
  );
  const current = empty || found.query === text.trim();
  const visible = current ? items : [];
  const selected = visible[active];
  const loading = empty ? recent.loading : !current || found.loading;
  const showRecent = empty && p.recent !== false;
  const emptyMessage = empty ? !header && showRecent && !recent.error ? "contents.noRecentSearches" : "contents.searchHint" : "contents.noResults";
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node) && !panel.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  const reveal = () => {
    setOpen(true);
  };
  const search = (q: string) => {
    setOpen(false);
    if (q) p.onSearch(q);
  };
  const go = (s: Suggestion) => {
    if (s.type === "recent") {
      setText(s.query);
      return search(s.query);
    }
    const url = p.href ? p.href(s.type === "content" ? s.card : s.group) : s.type === "content" ? s.card.path : s.group.path;
    setOpen(false);
    if (!url) return;
    if (navigate) navigate(url);
    else location.assign(url);
  };
  const panelKeyDown = (event: KeyboardEvent) => {
    if (event.key !== "Escape") return;
    event.preventDefault();
    input.current?.focus();
    setOpen(false);
    setActive(-1);
  };
  const results = (<>
    {showRecent && (
      <div className={cn("flex items-center justify-between gap-2 text-muted-foreground", header ? "mb-4 text-base font-semibold" : "px-2 py-1 text-xs")}>
        <span id={`${id}-recent`} className="flex items-center gap-2">{header && <HugeiconsIcon icon={Clock01Icon} strokeWidth={2} className="size-5" aria-hidden />}{t("contents.recentSearches")}</span>
        {visible.length > 0 && <button
          type="button"
          className="rounded px-1 hover:text-foreground"
          onMouseDown={(e) => {
            e.preventDefault();
          }}
          onClick={() => void recent.clear()}
        >
          {t(header ? "contents.clearAllRecent" : "contents.clearRecent")}
        </button>}
      </div>
    )}
    <ul id={`${id}-list`} role="listbox" aria-label={empty ? t("contents.recentSearches") : t("contents.suggestions")} className={cn("grid", header && "gap-2")}>
      {visible.map((s, i) => (
        <li
          key={s.type === "content" ? `c:${s.card.id}` : s.type === "group" ? `g:${s.group.id}` : `r:${s.query}`}
          id={`${id}-${i}`}
          role="option"
          aria-selected={i === active}
          data-ckui="suggestion"
          data-type={s.type}
          className={cn("flex cursor-pointer items-center", header ? s.type === "recent" ? "gap-2 rounded-md p-3 text-base hover:bg-foreground/10" : "gap-3 rounded-md px-4 py-1 text-base font-medium hover:bg-foreground/10" : "gap-2 px-2 py-1.5", i === active && "bg-accent text-accent-foreground")}
          onMouseEnter={() => setActive(i)}
          onMouseDown={(e) => {
            e.preventDefault();
            go(s);
          }}
        >
          {header && s.type !== "recent" ? (
            <>
              <SearchTypeIcon kind={s.type === "content" ? s.card.kind : s.group.kind} />
              <span className="sr-only">{labelOf(p.labels, s.type === "content" ? s.card.kind : s.group.kind)}</span>
              <span className="min-w-0 flex-1 truncate">{s.type === "content" ? s.card.title || s.card.code : s.group.name || s.group.slug}</span>
            </>
          ) : s.type === "content" ? (
            <>
              <span className="h-10 w-8 shrink-0 overflow-hidden rounded bg-muted">
                <RenditionImg outputs={s.card.cover} alt="" fit="cover" className="size-full" />
              </span>
              <span className="min-w-0 truncate">{s.card.title || s.card.code}</span>
            </>
          ) : s.type === "group" ? (
            <>
              <span className="min-w-0 flex-1 truncate">{s.group.name || s.group.slug}</span>
              <span className="text-xs text-muted-foreground">{labelOf(p.labels, s.group.kind)}</span>
            </>
          ) : (
            <>
              <HugeiconsIcon icon={header ? Search01Icon : Clock01Icon} strokeWidth={2} className="size-4 shrink-0 text-muted-foreground" aria-hidden />
              <span className="min-w-0 flex-1 truncate">{s.query}</span>
              {header && <SearchDate value={s.searchedAt} formatter={dates} />}
              <button
                type="button"
                className="rounded p-0.5 text-muted-foreground hover:text-foreground"
                aria-label={t("contents.removeRecent", { q: s.query })}
                onMouseDown={(e) => {
                  e.preventDefault();
                  e.stopPropagation();
                }}
                onClick={() => void recent.clear(s.query)}
              >
                <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} className="size-3.5" aria-hidden />
              </button>
            </>
          )}
        </li>
      ))}
    </ul>
    {loading ? <div role="status" className={cn("text-muted-foreground", header ? "flex justify-center py-4" : "px-3 py-2")}>{header ? <><span className="size-6 animate-spin rounded-full border-2 border-muted border-t-foreground motion-reduce:animate-none" aria-hidden /><span className="sr-only">{t("contents.loading")}</span></> : t("contents.loading")}</div>
      : !empty && found.error ? <p role="alert" className={cn("px-3 py-2 text-destructive", header && "text-center")}>{error(found.error)}</p>
      : visible.length === 0 && <div role="status" className={cn("text-muted-foreground", header ? "flex flex-col items-center gap-2 py-8 text-center" : "px-3 py-2")}>{header && <HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="size-8" aria-hidden />}<p>{t(emptyMessage, { q: text.trim() })}</p></div>}
  </>);
  return (
    <ContentKitUiRoot ref={root} appearance={p.appearance} className={cn("relative text-sm", p.className)} data-ckui="search-box" onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget) && !panel.current?.contains(event.relatedTarget)) setOpen(false); }}>
      <form
        role="search"
        className="relative"
        onSubmit={(e) => {
          e.preventDefault();
          if (open && selected) return go(selected);
          search(text.trim());
        }}
      >
        {header ? <button type="submit" className="absolute top-1/2 left-2.5 -translate-y-1/2 rounded text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring" aria-label={t("contents.search")}><HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="size-4" aria-hidden /></button>
          : <HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden />}
        <Input
          ref={input}
          type={header ? "text" : "search"}
          role="combobox"
          aria-label={t("contents.search")}
          aria-expanded={open}
          aria-controls={`${id}-list`}
          aria-activedescendant={open && selected ? `${id}-${active}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          className={cn("h-9 ps-8", header && "rounded-full border-border/50 bg-foreground/10 pe-9 text-base shadow-none md:text-base")}
          value={text}
          placeholder={p.placeholder ?? t("contents.searchPlaceholder")}
          onChange={(e) => {
            setText(e.target.value);
            setOpen(true);
            setActive(-1);
          }}
          onFocus={reveal}
          onClick={reveal}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              setOpen(false);
              setActive(-1);
              return;
            }
            if (e.key === "ArrowDown") {
              e.preventDefault();
              reveal();
              setActive((a) => open ? Math.min(visible.length - 1, a + 1) : 0);
            } else if (e.key === "ArrowUp" && open) {
              e.preventDefault();
              setActive((a) => Math.max(-1, a - 1));
            }
          }}
        />
        {header && (text || open) && (
          <button
            type="button"
            className="absolute top-1/2 right-2.5 -translate-y-1/2 rounded p-0.5 text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
            aria-label={text ? t("contents.clearSearch") : t("common.close")}
            onMouseDown={(event) => event.preventDefault()}
            onClick={() => {
              input.current?.focus();
              setActive(-1);
              if (text) { setText(""); setOpen(true); }
              else setOpen(false);
            }}
          ><HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} className="size-4" aria-hidden /></button>
        )}
      </form>
      {open && (header ? createPortal(
        <ContentKitUiRoot appearance={p.appearance} className="fixed inset-x-0 bottom-0 z-[70] text-sm" style={{ top }} data-ckui="search-layer">
          <div className="absolute inset-0 bg-black/30 backdrop-blur-sm" data-ckui="search-backdrop" aria-hidden />
          <div ref={panel} className="relative w-full bg-background pt-1.5 text-foreground shadow-lg" data-ckui="search-panel" onKeyDown={panelKeyDown}>
            <div className="mx-auto w-full max-w-[71rem] px-4 pt-5 md:px-[10%]">
              <div className="[--search-max-height:70vh] overflow-y-auto pb-7 md:[--search-max-height:60vh]" style={{ maxHeight: `min(var(--search-max-height), calc(100dvh - ${top}px - 1.875rem))` }}>
                <div className={empty ? "px-4 py-4" : "px-4"}>{results}</div>
              </div>
            </div>
          </div>
        </ContentKitUiRoot>, document.body,
      ) : <div ref={panel} className="absolute top-full z-20 mt-1 grid max-h-96 w-full overflow-auto rounded-md border border-border bg-popover py-1 text-popover-foreground shadow-md" data-ckui="search-panel" onKeyDown={panelKeyDown}>{results}</div>)}

    </ContentKitUiRoot>
  );
}

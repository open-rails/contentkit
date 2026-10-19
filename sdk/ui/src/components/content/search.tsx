import { Cancel01Icon, Clock01Icon, File01Icon, Folder01Icon, Image01Icon, Search01Icon, TagsIcon, UserIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon, type IconSvgElement } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type RefObject, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { TypeaheadQuery } from "../../client/discovery.js";
import type { Card, GroupCard, Typeahead } from "../../client/generated/wire.js";
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
  /** Matching recent searches first, with prefixes before interior matches. Default true. */
  recent?: boolean;
  /** An authenticated or host-specific suggestion source; public typeahead by default. */
  requestSuggestions?: (query: TypeaheadQuery, signal: AbortSignal) => Promise<Typeahead>;
  /** Enter opens an explicitly selected suggestion; free text never submits a results search. */
  selectionOnly?: boolean;
  disabled?: boolean;
  /** Called after a nonempty query or navigable suggestion is chosen. */
  onSelect?: () => void;
  /** Header portals below the field; dialog keeps suggestions in the layout. */
  presentation?: "inline" | "header" | "dialog";
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

type Suggestion = { type: "content"; card: Card } | { type: "group"; group: GroupCard } | { type: "recent"; query: string };
const suggestionKey = (s: Suggestion) => s.type === "content" ? `c:${s.card.id}` : s.type === "group" ? `g:${s.group.id}` : `r:${s.query}`;

// Mirror search-only normalization. Identity/slug normalization stays separate;
// complete English filler words never change Japanese/CJK or interior letters.
const searchText = (text: string) => text.normalize("NFKD").replace(/[\u0300-\u036f]/g, "").normalize("NFC").toLowerCase()
  .replace(/(^|[^\p{L}\p{N}\p{M}_])(?:the|a|an)(?=$|[^\p{L}\p{N}\p{M}_])/gu, "$1").trim().replace(/\s+/g, " ");

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

function useHeaderAnchor(open: boolean, enabled: boolean, root: RefObject<HTMLDivElement | null>, input: RefObject<HTMLInputElement | null>) {
  const [position, setPosition] = useState({ top: 0, left: 0, width: 0 });
  useLayoutEffect(() => {
    if (!open || !enabled || !root.current) return;
    const anchor = input.current ?? root.current;
    const view = root.current.ownerDocument.defaultView;
    if (!view) return;
    const measure = () => {
      const rect = anchor.getBoundingClientRect();
      setPosition({ top: Math.max(0, Math.min(view.innerHeight, rect.bottom + 4)), left: rect.left, width: rect.width });
    };
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
  return position;
}

function SearchTypeIcon({ kind }: { kind: string }) {
  const type = TYPE_ICONS[kind];
  return <HugeiconsIcon icon={type?.icon ?? Search01Icon} strokeWidth={2} className={cn("size-4 shrink-0", type?.color ?? "text-muted-foreground")} data-search-kind={kind} aria-hidden />;
}

/**
 * Matching recent queries first, followed by entity/title substrings. Prefixes
 * are preferred. Enter searches or opens the chosen suggestion; full search can
 * match contextual fields more deeply.
 */
export function SearchBox(p: SearchBoxProps) {
  const { t, error } = useMessages();
  const id = useId();
  const navigate = useContext(ContentKitContext)?.navigate;
  const [text, setText] = useState(p.defaultValue ?? "");
  const [open, setOpen] = useState(false);
  // null requests the first result when opening an as-yet-unloaded panel.
  const [active, setActive] = useState<string | null>();
  const root = useRef<HTMLDivElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const header = p.presentation === "header";
  const dialog = p.presentation === "dialog";
  const anchor = useHeaderAnchor(open, header, root, input);
  const { top } = anchor;
  const empty = !text.trim();
  const found = useTypeahead(text, { kind: p.kind, entity_kinds: p.groupKinds, client: p.client, request: p.requestSuggestions, minLength: p.disabled ? Infinity : 1 });
  const recent = useRecentSearches({ enabled: p.recent !== false && open && !p.disabled, client: p.client });
  const normalized = searchText(text);
  const current = found.query === text.trim();
  const visible = useMemo<Suggestion[]>(
    () => [
      ...(p.recent === false ? [] : recent.searches
        .map((r) => ({ ...r, normalized: searchText(r.query) }))
        .filter((r) => empty || (normalized && r.normalized.includes(normalized)))
        .sort((a, b) => Number(b.normalized.startsWith(normalized)) - Number(a.normalized.startsWith(normalized)))
        .map((r) => ({ type: "recent" as const, query: r.query }))),
      ...(!empty && current ? [
        ...found.entities.map((group) => ({ type: "group" as const, group })),
        ...found.contents.map((card) => ({ type: "content" as const, card })),
      ] : []),
    ],
    [p.recent, normalized, empty, current, recent.searches, found.contents, found.entities],
  );
  // History and live suggestions can arrive independently. Track identity so
  // prepending history never changes what Enter will open.
  const activeIndex = active === null ? Math.min(0, visible.length - 1) : visible.findIndex((s) => suggestionKey(s) === active);
  const selected = visible[activeIndex];
  const activate = (index: number) => {
    const suggestion = visible[index];
    setActive(suggestion ? suggestionKey(suggestion) : index >= 0 ? null : undefined);
  };
  const loading = recent.loading || (!empty && (!current || found.loading));
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
    if (!p.disabled) setOpen(true);
  };
  const search = (q: string) => {
    setOpen(false);
    if (q) { p.onSelect?.(); p.onSearch(q); }
  };
  const go = (s: Suggestion) => {
    if (s.type === "recent") {
      setText(s.query);
      return search(s.query);
    }
    const url = p.href ? p.href(s.type === "content" ? s.card : s.group) : s.type === "content" ? s.card.path : s.group.path;
    setOpen(false);
    if (!url) return;
    p.onSelect?.();
    if (navigate) navigate(url);
    else location.assign(url);
  };
  const panelKeyDown = (event: KeyboardEvent) => {
    if (event.key !== "Escape") return;
    event.preventDefault();
    input.current?.focus();
    setOpen(false);
    setActive(undefined);
  };
  const results = (<>
    {showRecent && (
      <div className={cn("flex items-center justify-between gap-2 text-muted-foreground", header ? "mb-4 text-base font-semibold" : dialog ? "px-2 pt-1 pb-3 text-xs font-medium" : "px-2 py-1 text-xs")}>
        <span id={`${id}-recent`} className="flex items-center gap-2">{header && <HugeiconsIcon icon={Clock01Icon} strokeWidth={2} className="size-5" aria-hidden />}{t("contents.recentSearches")}</span>
        {visible.length > 0 && <button
          type="button"
          className={cn("rounded px-1 hover:text-foreground", dialog && "min-h-11 -my-3")}
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
          key={suggestionKey(s)}
          id={`${id}-${i}`}
          role="option"
          aria-label={header && s.type !== "recent" ? `${labelOf(p.labels, s.type === "content" ? s.card.kind : s.group.kind)} ${s.type === "content" ? s.card.title || s.card.code : s.group.name || s.group.slug}` : undefined}
          aria-selected={i === activeIndex}
          data-ckui="suggestion"
          data-type={s.type}
          className={cn("flex cursor-pointer items-center", header ? s.type === "recent" ? "gap-2 rounded-md p-3 text-base hover:bg-foreground/10" : "gap-3 rounded-md px-4 py-1 text-base font-medium hover:bg-foreground/10" : dialog ? "min-h-11 gap-3 rounded-md p-2 text-sm hover:bg-accent hover:text-accent-foreground" : "gap-2 px-2 py-1.5", i === activeIndex && "bg-accent text-accent-foreground")}
          onMouseEnter={() => setActive(suggestionKey(s))}
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
              <button
                type="button"
                className={cn("rounded p-0.5 text-muted-foreground hover:text-foreground", dialog && "-my-2 grid size-11 shrink-0 place-items-center")}
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
    {loading ? <div role="status" className={cn("text-muted-foreground", header ? "flex justify-center py-4" : dialog ? "px-2 py-8 text-center" : "px-3 py-2")}>{header ? <><span className="size-6 animate-spin rounded-full border-2 border-muted border-t-foreground motion-reduce:animate-none" aria-hidden /><span className="sr-only">{t("contents.loading")}</span></> : t("contents.loading")}</div>
      : !empty && found.error ? <p role="alert" className={cn("px-3 py-2 text-destructive", (header || dialog) && "px-2 py-8 text-center")}>{error(found.error)}</p>
      : visible.length === 0 && <div role="status" className={cn("text-muted-foreground", header ? "flex flex-col items-center gap-2 py-8 text-center" : dialog ? "px-2 py-8 text-center" : "px-3 py-2")}>{header && <HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="size-8" aria-hidden />}<p>{t(emptyMessage, { q: text.trim() })}</p></div>}
  </>);
  return (
    <ContentKitUiRoot ref={root} appearance={p.appearance} className={cn("relative text-sm", dialog && "flex min-h-0 flex-col gap-4", p.className)} data-ckui="search-box" onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget) && !panel.current?.contains(event.relatedTarget)) setOpen(false); }}>
      <form
        role="search"
        className="relative shrink-0"
        onSubmit={(e) => {
          e.preventDefault();
          if (p.disabled) return;
          if (open && selected) return go(selected);
          if (p.selectionOnly) return;
          search(text.trim());
        }}
      >
        {header ? <button type="submit" disabled={p.disabled} className="absolute top-1/2 left-2.5 -translate-y-1/2 rounded text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring" aria-label={t("contents.search")}><HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="size-4" aria-hidden /></button>
          : <HugeiconsIcon icon={Search01Icon} strokeWidth={2} className={cn("pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground", dialog && "left-3.5 size-4.5")} aria-hidden />}
        <Input
          ref={input}
          type={header ? "text" : "search"}
          role="combobox"
          aria-label={t("contents.search")}
          aria-expanded={open}
          aria-controls={`${id}-list`}
          aria-activedescendant={open && selected ? `${id}-${activeIndex}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          disabled={p.disabled}
          className={cn("h-9 ps-8", dialog && "h-12 border-border bg-background ps-10 text-base shadow-none focus-visible:ring-2 focus-visible:ring-ring/25 md:text-base dark:bg-background", header && "rounded-full border-border/50 bg-foreground/10 pe-9 text-base shadow-none md:text-base")}
          value={text}
          placeholder={p.placeholder ?? t("contents.searchPlaceholder")}
          onChange={(e) => {
            setText(e.target.value);
            setOpen(true);
            setActive(undefined);
          }}
          onFocus={reveal}
          onClick={reveal}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              setOpen(false);
              setActive(undefined);
              return;
            }
            if (e.key === "ArrowDown") {
              e.preventDefault();
              reveal();
              activate(open ? Math.min(visible.length - 1, activeIndex + 1) : 0);
            } else if (e.key === "ArrowUp" && open) {
              e.preventDefault();
              activate(Math.max(-1, activeIndex - 1));
            }
          }}
        />
        {header && (text || open) && (
          <button
            type="button"
            disabled={p.disabled}
            className="absolute top-1/2 right-2.5 -translate-y-1/2 rounded p-0.5 text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
            aria-label={text ? t("contents.clearSearch") : t("common.close")}
            onMouseDown={(event) => event.preventDefault()}
            onClick={() => {
              input.current?.focus();
              setActive(undefined);
              if (text) { setText(""); setOpen(true); }
              else setOpen(false);
            }}
          ><HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} className="size-4" aria-hidden /></button>
        )}
      </form>
      {open && (header ? createPortal(
        <ContentKitUiRoot appearance={p.appearance} className="fixed inset-x-0 bottom-0 z-[70] text-sm" style={{ top }} data-ckui="search-layer">
          <div className="absolute inset-0 bg-black/30" data-ckui="search-backdrop" aria-hidden />
          <div ref={panel} style={{ width: anchor.width, marginLeft: anchor.left }} className="relative origin-top animate-[search-expand_300ms_ease-out] rounded-lg border border-border bg-background text-foreground shadow-lg transition-none motion-reduce:animate-none" data-ckui="search-panel" onKeyDown={panelKeyDown}>
            <div className="w-full px-3 pt-3">
              <div className="[--search-max-height:70vh] overflow-y-auto pb-7 md:[--search-max-height:60vh]" style={{ maxHeight: `min(var(--search-max-height), calc(100dvh - ${top}px - 1.875rem))` }}>
                <div>{results}</div>
              </div>
            </div>
          </div>
        </ContentKitUiRoot>, document.body,
      ) : <div ref={panel} className={cn("absolute top-full z-20 mt-1 grid max-h-96 w-full overflow-auto rounded-md border border-border bg-popover py-1 text-popover-foreground shadow-md", dialog && "relative top-auto z-auto m-0 block min-h-[min(9rem,20dvh)] max-h-[min(24rem,50dvh)] border-0 bg-transparent p-2 shadow-none overscroll-contain")} data-ckui="search-panel" onKeyDown={panelKeyDown}>{results}</div>)}

    </ContentKitUiRoot>
  );
}

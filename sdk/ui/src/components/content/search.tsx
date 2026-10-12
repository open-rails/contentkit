import { Search01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useId, useMemo, useState } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Card, GroupCard } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext } from "../../react/context.js";
import { useTypeahead } from "../../react/discovery.js";
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
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

type Suggestion = { type: "content"; card: Card } | { type: "group"; group: GroupCard };

/** A search field suggesting contents and groups as the reader types; Enter searches, or opens the suggestion chosen. */
export function SearchBox(p: SearchBoxProps) {
  const { t } = useMessages();
  const id = useId();
  const navigate = useContext(ContentKitContext)?.navigate;
  const [text, setText] = useState(p.defaultValue ?? "");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const found = useTypeahead(text, { kind: p.kind, group_kinds: p.groupKinds, client: p.client });
  const items = useMemo<Suggestion[]>(
    () => [...found.contents.map((card) => ({ type: "content" as const, card })), ...found.groups.map((group) => ({ type: "group" as const, group }))],
    [found.contents, found.groups],
  );
  const shown = open && items.length > 0 && found.query === text.trim();
  const urlOf = (s: Suggestion) => (p.href ? p.href(s.type === "content" ? s.card : s.group) : s.type === "content" ? s.card.path : s.group.path);
  const go = (s: Suggestion) => {
    const url = urlOf(s);
    setOpen(false);
    if (!url) return;
    if (navigate) navigate(url);
    else location.assign(url);
  };
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("relative text-sm", p.className)} data-ckui="search-box">
      <form
        role="search"
        onSubmit={(e) => {
          e.preventDefault();
          if (shown && active >= 0) return go(items[active]!);
          const q = text.trim();
          setOpen(false);
          if (q) p.onSearch(q);
        }}
      >
        <HugeiconsIcon icon={Search01Icon} strokeWidth={2} className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden />
        <Input
          type="search"
          role="combobox"
          aria-label={t("contents.search")}
          aria-expanded={shown}
          aria-controls={`${id}-list`}
          aria-activedescendant={shown && active >= 0 ? `${id}-${active}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          className="h-9 ps-8"
          value={text}
          placeholder={p.placeholder ?? t("contents.searchPlaceholder")}
          onChange={(e) => {
            setText(e.target.value);
            setOpen(true);
            setActive(-1);
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => setOpen(false)}
          onKeyDown={(e) => {
            if (e.key === "Escape") return setOpen(false);
            if (!shown) return;
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setActive((a) => Math.min(items.length - 1, a + 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setActive((a) => Math.max(-1, a - 1));
            }
          }}
        />
      </form>
      {shown && (
        <ul id={`${id}-list`} role="listbox" aria-label={t("contents.suggestions")} className="absolute top-full z-20 mt-1 grid max-h-96 w-full overflow-auto rounded-md border border-border bg-popover py-1 text-popover-foreground shadow-md">
          {items.map((s, i) => (
            <li
              key={s.type === "content" ? `c:${s.card.id}` : `g:${s.group.id}`}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              data-ckui="suggestion"
              data-type={s.type}
              className={cn("flex cursor-pointer items-center gap-2 px-2 py-1.5", i === active && "bg-accent text-accent-foreground")}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                go(s);
              }}
            >
              {s.type === "content" ? (
                <>
                  <span className="h-10 w-8 shrink-0 overflow-hidden rounded bg-muted">
                    <RenditionImg outputs={s.card.cover} alt="" fit="cover" className="size-full" />
                  </span>
                  <span className="min-w-0 truncate">{s.card.title || s.card.code}</span>
                </>
              ) : (
                <>
                  <span className="min-w-0 flex-1 truncate">{s.group.name || s.group.slug}</span>
                  <span className="text-xs text-muted-foreground">{labelOf(p.labels, s.group.kind)}</span>
                </>
              )}
            </li>
          ))}
        </ul>
      )}
    </ContentKitUiRoot>
  );
}

import { cn } from "cn";
import { useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type MouseEvent, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { fold, humanize, letterOf } from "../../client/group-fields.js";
import type { Cover, GroupCard, GroupDetail, PublicRendition } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useContentKitClient } from "../../react/context.js";
import { Input } from "#ckui/ui/input";
import { RenditionImg } from "../rendition-img.js";
import { ErrorLine } from "../social/parts.js";

/** A group as covers and links need it. */
export type GroupLike = Pick<GroupCard, "id" | "kind" | "name" | "slug" | "path"> & { cover?: Cover };

/** Display names of kinds, attributes, roles and values by identifier; a missing one shows the identifier humanized. */
export type Labels = Readonly<Record<string, string>>;

export const labelOf = (labels: Labels | undefined, id: string) => labels?.[id] ?? humanize(id);

/** A language's name in the UI's language ("ja" → "Japanese"). */
export function useLanguageName(): (code: string) => string {
  const { language } = useMessages();
  return useMemo(() => {
    let names: Intl.DisplayNames | undefined;
    try {
      names = new Intl.DisplayNames(language ? [language] : undefined, { type: "language" });
    } catch {
      names = undefined;
    }
    return (code: string) => {
      try {
        return names?.of(code) ?? code;
      } catch {
        return code;
      }
    };
  }, [language]);
}

// Until cards carry cover renditions (ContentKit #120 media lane).
type WithRenditions = Cover & { renditions?: PublicRendition[] };

export interface GroupCoverProps {
  group: GroupLike;
  /** Shown instead, e.g. a cover just uploaded. */
  src?: string;
  /** Temporary: the host's cover until group cards carry renditions. */
  renderCover?: (group: GroupLike) => ReactNode;
  className?: string;
}

/** A group's cover: its renditions, else the host's, else its initial on a muted tile. */
export function GroupCover({ group, src, renderCover, className }: GroupCoverProps) {
  const box = cn("overflow-hidden rounded-md bg-muted", className);
  const initial = <span className={cn(box, "grid place-items-center text-lg font-semibold text-muted-foreground")}>{(group.name || group.slug).trim().charAt(0).toUpperCase() || "?"}</span>;
  if (src) return <img src={src} alt="" className={cn(box, "object-cover")} data-ckui="group-cover" />;
  const renditions = (group.cover as WithRenditions | undefined)?.renditions;
  if (renditions?.length) return <RenditionImg outputs={renditions} fit="cover" skeleton className={box} fallback={initial} data-ckui="group-cover" />;
  const own = renderCover?.(group);
  if (own) return <>{own}</>;
  return initial;
}

/** A link to a group's page: `href`, else its canonical path; in-app through the provider's navigate. */
export function GroupLink({ group, href, className, children }: { group: GroupLike; href?: (g: GroupLike) => string | undefined; className?: string; children: ReactNode }) {
  const navigate = useContext(ContentKitContext)?.navigate;
  const url = href ? href(group) : group.path;
  const onClick = useCallback(
    (e: MouseEvent<HTMLAnchorElement>) => {
      if (!navigate || !url || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      navigate(url);
    },
    [navigate, url],
  );
  if (!url) return <span className={className}>{children}</span>;
  return (
    <a href={url} className={className} onClick={onClick}>
      {children}
    </a>
  );
}

export interface GroupPickerProps {
  kind: string;
  /** Names in this language. */
  language?: string;
  /** Groups not offered, by id. */
  exclude?: readonly string[];
  label: string;
  onPick: (group: GroupCard) => void;
  client?: ContentKitClient;
  autoFocus?: boolean;
  className?: string;
}

/** Text matches a group when its name or slug starts a word with it. */
const matches = (g: GroupCard, q: string) => [fold(g.name), g.slug.toLowerCase()].some((s) => s.startsWith(q) || s.includes(` ${q}`) || s.includes(`-${q}`));

/**
 * A typeahead over one kind's groups. It reads the letter bucket of what is
 * typed and matches names and slugs in it, until discovery's typeahead serves groups.
 */
export function GroupPicker({ kind, language, exclude, label, onPick, client: own, autoFocus, className }: GroupPickerProps) {
  const { t } = useMessages();
  const client = useContentKitClient(own);
  const id = useId();
  const [text, setText] = useState("");
  const [bucket, setBucket] = useState<GroupCard[]>([]);
  const [active, setActive] = useState(0);
  const [error, setError] = useState<ContentKitError | null>(null);
  const cache = useRef(new Map<string, GroupCard[]>());
  const q = fold(text);
  const letter = q ? letterOf(q) : "";
  useEffect(() => {
    cache.current.clear();
  }, [kind, language, client]);
  useEffect(() => {
    if (!letter) return;
    const hit = cache.current.get(letter);
    if (hit) return setBucket(hit);
    const ctl = new AbortController();
    const timer = setTimeout(() => {
      client.groups.list({ kind, letter, language, sort: "name", limit: 200 }, ctl.signal).then(
        (page) => {
          cache.current.set(letter, page.groups);
          setError(null);
          setBucket(page.groups);
        },
        (e) => {
          const err = toContentKitError(e);
          if (err.code !== "aborted") setError(err);
        },
      );
    }, 150);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [letter, kind, language, client]);
  const results = useMemo(() => (q ? bucket.filter((g) => !exclude?.includes(g.id) && matches(g, q)).slice(0, 8) : []), [bucket, q, exclude]);
  useEffect(() => setActive(0), [q]);
  const pick = (g: GroupCard) => {
    setText("");
    onPick(g);
  };
  const open = results.length > 0;
  return (
    <div className={cn("relative grid min-w-48 flex-1 gap-1", className)} data-ckui="group-picker">
      <Input
        role="combobox"
        aria-label={label}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        aria-autocomplete="list"
        autoComplete="off"
        className="h-8"
        value={text}
        placeholder={t("groups.searchPlaceholder")}
        autoFocus={autoFocus}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (!open) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setActive((a) => Math.min(results.length - 1, a + 1));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => Math.max(0, a - 1));
          } else if (e.key === "Enter") {
            e.preventDefault();
            pick(results[active]!);
          } else if (e.key === "Escape") setText("");
        }}
      />
      {open && (
        <ul id={`${id}-list`} role="listbox" aria-label={label} className="absolute top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
          {results.map((g, i) => (
            <li
              key={g.id}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cn("flex cursor-pointer items-baseline justify-between gap-2 px-3 py-1.5", i === active && "bg-accent text-accent-foreground")}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(g);
              }}
            >
              <span className="min-w-0 truncate">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground">{g.slug}</span>
            </li>
          ))}
        </ul>
      )}
      {q && !open && bucket.length > 0 && !error && (
        <p className="text-xs text-muted-foreground" role="status">
          {t("groups.noMatch")}
        </p>
      )}
      <ErrorLine error={error} />
    </div>
  );
}

/** Groups read by id, for names where only ids are at hand (edges); a missing or deleted one is left out. */
export function useGroupNames(ids: readonly string[], o: { language?: string; client?: ContentKitClient } = {}): ReadonlyMap<string, GroupDetail> {
  const client = useContentKitClient(o.client);
  const [found, setFound] = useState<ReadonlyMap<string, GroupDetail>>(new Map());
  const key = [...new Set(ids)].sort().join(",");
  useEffect(() => {
    if (!key) return;
    const ctl = new AbortController();
    const list = key.split(",");
    // A merged group answers as its survivor: keep it under the id asked for.
    Promise.all(list.map((id) => client.groups.get(id, o.language, ctl.signal).catch(() => null))).then((all) => {
      if (!ctl.signal.aborted) setFound(new Map(all.flatMap((g, i) => (g ? [[list[i]!, g] as const] : []))));
    });
    return () => ctl.abort();
  }, [key, client, o.language]);
  return found;
}

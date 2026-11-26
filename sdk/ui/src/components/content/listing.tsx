import { cn } from "cn";
import { useCallback, useMemo, useState } from "react";
import type { ContentQuery, FeaturedWindow, ListSort } from "../../client/discovery.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { MessageKey, Translator } from "../../i18n/messages.js";
import { useKinds } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { labelOf } from "../entities/parts.js";
import { ContentGrid, type CardListProps, type LockedCards } from "./grid.js";

/** A sort menu choice: a list sort, or popular over a featured window. */
export type SortChoice = Exclude<ListSort, "popular"> | `popular:${FeaturedWindow}`;

const SORTS: readonly SortChoice[] = ["newest", "popular:week", "popular:30d", "popular:all", "top_rated", "most_favorited", "title", "oldest"];
const ANY = "*";

/** A choice's query fields: popular:30d is sort popular over window 30d. */
export function choiceQuery(choice: SortChoice): Pick<ContentQuery, "sort" | "window"> {
  const [sort, window] = choice.split(":") as [ListSort, FeaturedWindow | undefined];
  return window ? { sort, window } : { sort, window: undefined };
}

/** A query's choice: popular without a window is the week. */
export function queryChoice(q: Pick<ContentQuery, "sort" | "window">): SortChoice | undefined {
  if (q.sort !== "popular") return q.sort;
  const w = q.window ?? "week";
  return (["week", "30d", "90d", "year", "all"] as const).includes(w as FeaturedWindow) ? (`popular:${w}` as SortChoice) : "popular:week";
}

/** A sort's name: "Newest", "Popular, this week", "Best match". */
export function sortLabel(m: Translator, sort: SortChoice | "popular" | "position" | "random" | "relevance"): string {
  const [base, period] = sort.split(":");
  const name = m.t(`listing.sort_${base}` as MessageKey);
  return period ? m.t("listing.sortPeriod", { sort: name, period: m.t(`listing.period_${period}` as MessageKey) }) : name;
}

export interface SortMenuProps<S extends string> {
  value: S;
  /** The sorts offered, in menu order. */
  sorts: readonly S[];
  onChange: (sort: S) => void;
  /** A sort's name; default the list sorts' names. */
  label?: (sort: S) => string;
  className?: string;
}

/** A list's sort control; hosts keep the choice (URL, remembered per page type). */
export function SortMenu<S extends string>({ value, sorts, onChange, label, className }: SortMenuProps<S>) {
  const m = useMessages();
  const name = label ?? ((s: S) => sortLabel(m, s as SortChoice));
  return (
    <Select value={value} onValueChange={(v) => onChange(v as S)}>
      <SelectTrigger size="sm" aria-label={m.t("listing.sort")} className={cn("comfortable:data-[size=sm]:h-10", className)} data-ckui="listing-sort">
        <SelectValue>{(v: string) => name(v as S)}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {sorts.map((s) => (
          <SelectItem key={s} value={s}>
            {name(s)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

export interface ContentListingProps extends CardListProps {
  /** The listed kind: its version traits and filterable attributes become filters, its sort the default. */
  kind?: string;
  /** The sort and filters; with onQueryChange the host keeps them (e.g. in the URL). Other fields (groups, a tag page's) pass through. A changed query starts again at page 1: a host keeping the page drops it with the query. */
  query?: ContentQuery;
  onQueryChange?: (query: ContentQuery) => void;
  /** The sorts offered; default newest, popular (this week, last 30 days, all time), top rated, most favorited, title, oldest. */
  sorts?: readonly SortChoice[];
  /** Cards the reader doesn't open: show (default), hide or collapse. */
  locked?: LockedCards;
  /** Offer a language filter; default when the site has several languages. */
  languages?: boolean;
  /** The traits and attributes offered; default every trait and filterable attribute of the kind. */
  filters?: readonly string[];
}

/** A listing page: sort, language, trait and attribute filters over a paged grid of a kind's contents. */
export function ContentListing({ kind, query: controlled, onQueryChange, sorts = SORTS, languages, filters, ...p }: ContentListingProps) {
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const { kinds: model } = useKinds({ client: p.client });
  const [own, setOwn] = useState<ContentQuery>({});
  const query = controlled ?? own;
  const set = useCallback(
    (next: ContentQuery) => {
      if (controlled === undefined) setOwn(next);
      onQueryChange?.(next);
    },
    [controlled, onQueryChange],
  );
  const spec = kind ? model?.kinds[kind] : undefined;
  const choice: SortChoice = (query.sort && queryChoice(query)) || (spec?.sort ? queryChoice({ sort: spec.sort as ListSort }) : undefined) || sorts[0] || "newest";
  const offered = (name: string) => !filters || filters.includes(name);
  const traits = (spec?.versions.traits ?? []).filter(offered);
  const attributes = Object.entries(spec?.attributes ?? {}).filter(([n, a]) => a.filterable && (a.values?.length || a.type === "bool") && offered(n));
  const languageFilter = (languages ?? (model?.languages.length ?? 0) > 1) && !!model?.languages.length;
  const attributeValue = (name: string) => query.attribute?.find((a) => a.startsWith(`${name}:`))?.slice(name.length + 1);
  const setAttribute = (name: string, value: string | undefined) =>
    set({ ...query, attribute: [...(query.attribute ?? []).filter((a) => !a.startsWith(`${name}:`)), ...(value === undefined ? [] : [`${name}:${value}`])] });
  const toggleTrait = (x: string) => {
    const on = query.traits?.includes(x);
    set({ ...query, traits: on ? query.traits!.filter((y) => y !== x) : [...(query.traits ?? []), x] });
  };
  const filtered = !!(query.language || query.traits?.length || query.attribute?.length);
  const list = useMemo(() => ({ ...query, kind: kind ?? query.kind, ...choiceQuery(choice) }), [query, kind, choice]);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="content-listing">
      <div role="toolbar" aria-label={t("listing.filters")} className="flex flex-wrap items-center gap-2">
        <SortMenu<SortChoice> value={choice} sorts={sorts} onChange={(s) => set({ ...query, ...choiceQuery(s) })} />
        {languageFilter && (
          <Select value={query.language ?? ANY} onValueChange={(v) => set({ ...query, language: v === ANY ? undefined : (v as string) })}>
            <SelectTrigger size="sm" aria-label={t("listing.language")} data-ckui="listing-language">
              <SelectValue>{(v: string) => (v === ANY ? t("listing.anyLanguage") : languageName(v))}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ANY}>{t("listing.anyLanguage")}</SelectItem>
              {model!.languages.map((l) => (
                <SelectItem key={l} value={l}>
                  {languageName(l)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        {attributes.map(([name, a]) => {
          const value = attributeValue(name);
          if (a.type === "bool")
            return (
              <Button key={name} size="sm" variant={value === "true" ? "default" : "outline"} aria-pressed={value === "true"} onClick={() => setAttribute(name, value === "true" ? undefined : "true")}>
                {labelOf(p.labels, name)}
              </Button>
            );
          return (
            <Select key={name} value={value ?? ANY} onValueChange={(v) => setAttribute(name, v === ANY ? undefined : (v as string))}>
              <SelectTrigger size="sm" aria-label={labelOf(p.labels, name)} data-ckui="listing-attribute" data-name={name}>
                <SelectValue>{(v: string) => (v === ANY ? labelOf(p.labels, name) : labelOf(p.labels, v))}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ANY}>{t("listing.any")}</SelectItem>
                {a.values!.map((v) => (
                  <SelectItem key={v} value={v}>
                    {labelOf(p.labels, v)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          );
        })}
        {traits.map((x) => {
          const on = !!query.traits?.includes(x);
          return (
            <Button key={x} size="sm" variant={on ? "default" : "outline"} aria-pressed={on} onClick={() => toggleTrait(x)} data-ckui="listing-trait">
              {labelOf(p.labels, x)}
            </Button>
          );
        })}
        {filtered && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              const { language: _l, traits: _t, attribute: _a, ...rest } = query;
              set(rest);
            }}
          >
            {t("listing.clear")}
          </Button>
        )}
      </div>
      <ContentGrid {...p} query={list} />
    </ContentKitUiRoot>
  );
}

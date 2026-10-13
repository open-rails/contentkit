import { cn } from "cn";
import { useCallback, useMemo, useState } from "react";
import type { ContentQuery, ListSort } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import type { MessageKey, Translator } from "../../i18n/messages.js";
import { useKinds } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { labelOf, useLanguageName } from "../groups/parts.js";
import { ContentGrid, type CardListProps } from "./grid.js";

const SORTS: readonly ListSort[] = ["newest", "popular:7d", "popular:30d", "popular", "top_rated", "most_favorited", "title", "oldest"];
const ANY = "*";

/** A sort's name: "Newest", "Popular, this week". */
export function sortLabel(m: Translator, sort: ListSort): string {
  const [base, period] = sort.split(":");
  const name = m.t(`listing.sort_${base}` as MessageKey);
  return period ? m.t("listing.sortPeriod", { sort: name, period: m.t(`listing.period_${period}` as MessageKey) }) : name;
}

export interface ContentListingProps extends CardListProps {
  /** The listed kind: its version traits and filterable attributes become filters, its sort the default. */
  kind?: string;
  /** The sort and filters; with onQueryChange the host keeps them (e.g. in the URL). Other fields (groups, a tag page's) pass through. */
  query?: ContentQuery;
  onQueryChange?: (query: ContentQuery) => void;
  /** The sorts offered; default newest, popular (week, month, all time), top rated, most favorited, title, oldest. */
  sorts?: readonly ListSort[];
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
  const sort = query.sort ?? (spec?.sort as ListSort | undefined) ?? sorts[0];
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
  const list = useMemo(() => ({ ...query, kind: kind ?? query.kind, sort }), [query, kind, sort]);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="content-listing">
      <div role="toolbar" aria-label={t("listing.filters")} className="flex flex-wrap items-center gap-2">
        <Select value={sort} onValueChange={(v) => set({ ...query, sort: v as ListSort })}>
          <SelectTrigger size="sm" aria-label={t("listing.sort")} data-ckui="listing-sort">
            <SelectValue>{(v: string) => sortLabel(m, v as ListSort)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {sorts.map((s) => (
              <SelectItem key={s} value={s}>
                {sortLabel(m, s)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
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

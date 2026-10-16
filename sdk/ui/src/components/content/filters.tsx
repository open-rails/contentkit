import { cn } from "cn";
import { useContext, useId } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ReleaseDatesQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, type ContentKitErrorHandler } from "../../react/context.js";
import { useContentFilters, useReleaseDates } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Label } from "#ckui/ui/label";
import { Skeleton } from "#ckui/ui/skeleton";
import { Switch } from "#ckui/ui/switch";
import { labelOf, type Labels } from "../groups/parts.js";
import { contentError, ErrorLine, useAction } from "../social/parts.js";

export interface ContentFilterSettingsProps {
  /** Content kinds to show. Omitted displays every configured kind. */
  kinds?: readonly string[];
  /** Display labels for content-kind sections, when more than one is shown. */
  labels?: Labels;
  /** Entity names in this language; default the provider's. */
  language?: string;
  /** Offer listing deleted content the reader holds. Default true. */
  includeDeleted?: boolean;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Show or hide configured content for this browser or signed-in account. */
export function ContentFilterSettings(p: ContentFilterSettingsProps) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const ctx = useContext(ContentKitContext);
  const f = useContentFilters({ language: p.language, client: p.client });
  const save = useAction("filters.save", p.onError);
  const root = (children: React.ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="content-filters">
      {children}
    </ContentKitUiRoot>
  );
  if (!f.filters) {
    if (f.error) return root(<ErrorLine>{contentError(m, f.error)}</ErrorLine>);
    return root(<Skeleton className="h-24 w-full" />);
  }
  const filters = f.filters;
  const groups = Object.entries(filters.kinds).filter(([kind, rows]) => rows.length && (!p.kinds || p.kinds.includes(kind)));
  if (!groups.length && p.includeDeleted === false) return null;
  return root(
    <>
      {groups.map(([kind, rows]) => (
        <section key={kind} className="grid gap-2" data-content-kind={kind}>
          {groups.length > 1 && <h4 className="font-medium">{labelOf(p.labels, kind)}</h4>}
          <ul className="grid divide-y divide-border rounded-lg border border-border">
            {rows.map((x) => (
              <li key={x.entity.id} className="flex flex-wrap items-center gap-3 p-3" data-ckui="content-filter" data-kind={kind} data-slug={x.entity.slug} data-visible={x.visible}>
                <Switch id={`${id}-${kind}-${x.entity.id}`} checked={x.visible} disabled={save.pending} onCheckedChange={(v) => void save.run(() => f.setVisible(kind, x.entity.id, v))} />
                <Label htmlFor={`${id}-${kind}-${x.entity.id}`} className="min-w-0 flex-1">
                  {x.entity.name || x.entity.slug}
                </Label>
              </li>
            ))}
          </ul>
        </section>
      ))}
      {p.includeDeleted !== false && ctx?.viewer !== null && (
        <div className="flex items-center gap-3">
          <Switch id={`${id}-deleted`} checked={filters.include_deleted} disabled={save.pending} onCheckedChange={(v) => void save.run(() => f.setIncludeDeleted(v))} />
          <Label htmlFor={`${id}-deleted`}>{t("contents.includeDeleted")}</Label>
        </div>
      )}
      <ErrorLine error={save.error} />
    </>,
  );
}

export interface ReleaseDateIndexProps {
  /** Which contents count: the listing's filters. */
  query?: ReleaseDatesQuery;
  /** The bucket chosen (a day YYYY-MM-DD or month YYYY-MM): list with released_on set to it. */
  selected?: string;
  onSelect?: (date: string) => void;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** The release date index: per month or day, newest first, how many contents and how many free. */
export function ReleaseDateIndex({ query = {}, selected, onSelect, client, className, appearance }: ReleaseDateIndexProps) {
  const m = useMessages();
  const { t } = m;
  const dates = useReleaseDates(query, { client });
  const label = (date: string) => {
    const [y, mo, d] = date.split("-").map(Number);
    const at = new Date(Date.UTC(y!, (mo ?? 1) - 1, d ?? 1));
    return new Intl.DateTimeFormat(m.language, d ? { dateStyle: "medium", timeZone: "UTC" } : { year: "numeric", month: "long", timeZone: "UTC" }).format(at);
  };
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-2 text-sm", className)} data-ckui="release-dates">
      {dates.loading && !dates.items.length ? (
        <Skeleton className="h-24 w-full" />
      ) : dates.error && !dates.items.length ? (
        <ErrorLine>{contentError(m, dates.error)}</ErrorLine>
      ) : !dates.items.length ? (
        <p className="text-muted-foreground">{t("contents.noDates")}</p>
      ) : (
        <ul className="grid gap-1">
          {dates.items.map((d) => (
            <li key={d.date}>
              <button
                type="button"
                aria-pressed={selected === d.date}
                className={cn("flex w-full items-center justify-between gap-3 rounded-md px-2 py-1.5 text-start hover:bg-muted", selected === d.date && "bg-muted font-medium")}
                onClick={() => onSelect?.(d.date)}
                data-ckui="release-date"
                data-date={d.date}
              >
                <span>{label(d.date)}</span>
                <span className="text-xs text-muted-foreground tabular-nums">
                  {m.plural("contents.items", d.count)}
                  {d.free < d.count && ` · ${m.plural("contents.free", d.free)}`}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {dates.hasMore && (
        <Button variant="outline" size="sm" className="justify-self-center" disabled={dates.loadingMore} onClick={() => void dates.loadMore()}>
          {t("contents.loadMore")}
        </Button>
      )}
    </ContentKitUiRoot>
  );
}

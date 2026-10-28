import { cn } from "cn";
import { useId } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { EntityCard } from "../../client/generated/wire.js";
import type { ReleaseDatesQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useContentFilters, useReleaseDates } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Label } from "#ckui/ui/label";
import { Skeleton } from "#ckui/ui/skeleton";
import { Switch } from "#ckui/ui/switch";
import { contentError, ErrorLine, useAction } from "../social/parts.js";

/** Counts for one concrete listing selection, aggregated by entity.
 * Only rows offered by the current domain's filter inventory are displayed. */
export interface ContentFilterCount {
  entity: Pick<EntityCard, "id">;
  count: number;
}

export interface ContentFilterSettingsProps {
  /** Listing context; shows shared controls when any kind is in applies_to. */
  kinds?: readonly string[];
  /** Entity names in this language; default the provider's. */
  language?: string;
  /** Currently excluded matches in this listing/day; omitted shows no counts. */
  counts?: readonly ContentFilterCount[];
  /** Counts cover a bounded candidate set, so each displayed count is a lower bound. */
  countsLowerBound?: boolean;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Show or hide configured content for this browser or signed-in account. */
export function ContentFilterSettings(p: ContentFilterSettingsProps) {
  const m = useMessages();
  const id = useId();
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
  const rows = !p.kinds || p.kinds.some((kind) => filters.applies_to.includes(kind)) ? filters.filters : [];
  if (!rows.length) return null;
  return root(
    <>
      <ul className="grid divide-y divide-border rounded-lg border border-border">
        {rows.map((x) => (
          <li key={x.entity.id} className="flex flex-wrap items-center gap-3 p-3" data-ckui="content-filter" data-slug={x.entity.slug} data-visible={x.visible}>
            <Switch id={`${id}-${x.entity.id}`} checked={x.visible} disabled={save.pending} onCheckedChange={(v) => void save.run(() => f.setVisible(x.entity.id, v))} />
            <Label htmlFor={`${id}-${x.entity.id}`} className="min-w-0 flex-1">
              {x.entity.name || x.entity.slug}
              {p.counts !== undefined && (
                <span className="tabular-nums">: {p.countsLowerBound ? "≥ " : ""}{new Intl.NumberFormat(m.language).format(p.counts.filter((entry) => entry.entity.id === x.entity.id).reduce((n, entry) => n + entry.count, 0))}</span>
              )}
            </Label>
          </li>
        ))}
      </ul>
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
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, dates.error)}</p>
          <Button size="sm" variant="outline" onClick={dates.reload}>
            {t("common.retry")}
          </Button>
        </div>
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
        <div className="grid justify-items-center gap-2">
          {dates.error && <p role="alert" className="text-destructive">{contentError(m, dates.error)}</p>}
          <Button variant="outline" size="sm" disabled={dates.loadingMore} onClick={() => void dates.loadMore()}>
            {dates.loadingMore ? t("contents.loading") : dates.error ? t("common.retry") : t("contents.loadMore")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

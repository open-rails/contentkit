import { cn } from "cn";
import { useMemo, useState } from "react";
import { popularQuery, type PopularDateRange, type PopularPeriod, type PopularQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { ContentGrid, type CardListProps, type LockedCards } from "./grid.js";

export interface PopularContentGridProps extends Omit<CardListProps, "infinite"> {
  query?: PopularQuery;
  /** Controlled activity window; otherwise the selector starts at 7d. Date ranges require query.kind. */
  period?: PopularPeriod | PopularDateRange;
  onPeriodChange?: (period: PopularPeriod) => void;
  locked?: LockedCards;
}

const PERIODS: readonly PopularPeriod[] = ["7d", "90d", "365d", "all"];

/** Rank activity in the selected window, across publication ages, with infinite paging. */
export function PopularContentGrid({ query, period, onPeriodChange, locked, ...p }: PopularContentGridProps) {
  const m = useMessages();
  const [ownPeriod, setOwnPeriod] = useState<PopularPeriod>("7d");
  const current = period ?? ownPeriod;
  const selected = typeof current === "string" ? current : "custom";
  const list = useMemo(() => popularQuery(current, query), [current, query]);
  const label = (value: PopularPeriod) => value === "all" ? m.t("listing.period_all") : m.plural("listing.pastDays", Number(value.slice(0, -1)));
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="popular-grid">
      <Tabs value={selected} onValueChange={(value) => {
        if (!value || !PERIODS.includes(value as PopularPeriod)) return;
        const next = value as PopularPeriod;
        if (period === undefined) setOwnPeriod(next);
        onPeriodChange?.(next);
      }} className="gap-4">
        <TabsList aria-label={m.t("listing.popularPeriod")} className="mx-auto grid w-full grid-cols-4 group-data-horizontal/tabs:h-auto sm:w-fit">
          {PERIODS.map((value) => (
            <TabsTrigger key={value} value={value} className="min-h-10 whitespace-normal px-3 py-2 text-center">
              {label(value)}
            </TabsTrigger>
          ))}
        </TabsList>
        <TabsContent value={selected}>
          {typeof current !== "string" && (
            <p className="mb-4 text-center text-muted-foreground">
              {m.t("listing.activityRange", { from: current.from, until: current.until })}
            </p>
          )}
          <ContentGrid {...p} surface={p.surface ?? "popular"} query={list} locked={locked} infinite />
        </TabsContent>
      </Tabs>
    </ContentKitUiRoot>
  );
}

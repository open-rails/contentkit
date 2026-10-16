import { cn } from "cn";
import { useMemo, useState } from "react";
import { popularQuery, type PopularPeriod, type PopularQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { ContentGrid, type CardListProps, type LockedCards } from "./grid.js";

export interface PopularContentGridProps extends Omit<CardListProps, "infinite"> {
  query?: PopularQuery;
  /** Controlled period; otherwise the selector starts at 7d. */
  period?: PopularPeriod;
  onPeriodChange?: (period: PopularPeriod) => void;
  locked?: LockedCards;
}

const PERIODS: readonly PopularPeriod[] = ["7d", "90d", "365d", "all"];

/** A popularity listing with matched score/age windows and infinite paging. */
export function PopularContentGrid({ query, period, onPeriodChange, locked, ...p }: PopularContentGridProps) {
  const m = useMessages();
  const [ownPeriod, setOwnPeriod] = useState<PopularPeriod>("7d");
  const current = period ?? ownPeriod;
  const list = useMemo(() => popularQuery(current, query), [current, query]);
  const label = (value: PopularPeriod) => value === "all" ? m.t("listing.period_all") : m.plural("listing.pastDays", Number(value.slice(0, -1)));
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="popular-grid">
      <Select value={current} onValueChange={(value) => {
        if (!value || !PERIODS.includes(value as PopularPeriod)) return;
        const next = value as PopularPeriod;
        if (period === undefined) setOwnPeriod(next);
        onPeriodChange?.(next);
      }}>
        <SelectTrigger className="w-fit min-w-40" aria-label={m.t("listing.popularPeriod")}><SelectValue>{label(current)}</SelectValue></SelectTrigger>
        <SelectContent>{PERIODS.map((value) => <SelectItem key={value} value={value}>{label(value)}</SelectItem>)}</SelectContent>
      </Select>
      <ContentGrid {...p} surface={p.surface ?? "popular"} query={list} locked={locked} infinite />
    </ContentKitUiRoot>
  );
}

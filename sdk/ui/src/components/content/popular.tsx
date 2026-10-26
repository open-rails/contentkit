import { cn } from "cn";
import { useMemo, useState } from "react";
import { popularQuery, type PopularDateRange, type PopularPeriod, type PopularQuery } from "../../client/discovery.js";
import type { PopularityBucket } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useKinds } from "../../react/entities.js";
import { useContents } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { CardGrid, cardListMode, type CardListProps } from "./grid.js";

const lastIncludedDay = (until:string) => new Date(Date.parse(until)-86_400_000).toISOString().slice(0,10);

export interface PopularContentGridProps extends Omit<CardListProps, "infinite"> {
  query: PopularQuery & { kind: string };
  /** A configured bucket ID; otherwise the first available host choice. */
  period?: PopularPeriod | PopularDateRange;
  onPeriodChange?: (period: PopularPeriod) => void;
}

/** Read the host's prepared ranking choices, independently of publication age. */
export function PopularContentGrid({ query, period, onPeriodChange, ...p }: PopularContentGridProps) {
  const m = useMessages();
  const { kinds } = useKinds({ client:p.client });
  const [ownPeriod,setOwnPeriod] = useState("");
  const choices = (kinds?.kinds[query.kind]?.popularity ?? []).filter(b =>
    !b.scopes?.length || b.scopes.some(s => !s.language || s.language === (query.language ?? "")));
  const current = period ?? (choices.some(b => b.id === ownPeriod) ? ownPeriod : choices[0]?.id);
  const selected = typeof current === "string" ? current : "custom";
  const label = (bucket:PopularityBucket) => bucket.label || (bucket.all ? m.t("listing.period_all") : bucket.days ? m.plural("listing.pastDays",bucket.days) : m.t("listing.activityRange",{from:bucket.from!,until:lastIncludedDay(bucket.until!)}));
  if (!current || !choices.length) return null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm",p.className)} data-ckui="popular-grid">
      <Tabs value={selected} onValueChange={value => {
        if (!value || !choices.some(b => b.id === value)) return;
        if (period === undefined) setOwnPeriod(value);
        onPeriodChange?.(value);
      }} className="gap-4">
        <TabsList aria-label={m.t("listing.popularPeriod")} className="mx-auto flex h-auto w-full flex-wrap sm:w-fit">
          {choices.map(bucket => <TabsTrigger key={bucket.id} value={bucket.id} className="min-h-10 whitespace-normal px-3 py-2 text-center">{label(bucket)}</TabsTrigger>)}
        </TabsList>
        <TabsContent value={selected}>
          <PreparedPopularGrid {...p} query={query} bucket={current} />
        </TabsContent>
      </Tabs>
    </ContentKitUiRoot>
  );
}

function PreparedPopularGrid({query,bucket,...p}:CardListProps & {query:PopularQuery;bucket:string | PopularDateRange}) {
  const m = useMessages();
  const q = useMemo(() => popularQuery(bucket,{...query,summary:query.summary ?? true,limit:query.limit ?? p.pageSize,mode:cardListMode(p.locked,query.mode)}),[bucket,query,p.pageSize,p.locked]);
  const list = useContents(q,{client:p.client});
  const snapshot = list.popularity;
  return <>
    {snapshot?.ready && snapshot.from && snapshot.until && <p className="mb-4 text-center text-muted-foreground">{m.t("listing.activityRange",{from:snapshot.from.slice(0,10),until:lastIncludedDay(snapshot.until)})}</p>}
    <CardGrid {...p} surface={p.surface ?? "popular"} list={list} render={JSON.stringify(q)} filterKinds={query.kind ? [query.kind] : undefined} filterLanguage={query.display_language ?? query.language} empty={snapshot && !snapshot.ready ? m.t("listing.preparingPopularity") : p.empty} infinite />
  </>;
}

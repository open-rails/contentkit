import { cn } from "cn";
import { useMemo, useState } from "react";
import { popularQuery, type FeaturedWindow, type PopularQuery, type PopularWindow } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import type { MessageKey } from "../../i18n/messages.js";
import { useKinds } from "../../react/entities.js";
import { useContents } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { CardGrid, cardListMode, type CardListProps } from "./grid.js";

const WINDOWS: readonly FeaturedWindow[] = ["week", "30d", "90d", "year", "all"];
const lastIncludedDay = (until: string) => new Date(Date.parse(until) - 86_400_000).toISOString().slice(0, 10);

export interface PopularContentGridProps extends Omit<CardListProps, "infinite"> {
  query: PopularQuery & { kind: string };
  /** The window shown: a featured one's tab, or any other window (a calendar month or year, a range of days) without a tab. The week by default. */
  window?: PopularWindow;
  onWindowChange?: (window: FeaturedWindow) => void;
  /** The featured windows offered as tabs, in order. */
  windows?: readonly FeaturedWindow[];
}

/** A kind's popular contents over a window, with a tab per featured window. Shown while the site records signals. */
export function PopularContentGrid({ query, window, onWindowChange, windows = WINDOWS, ...p }: PopularContentGridProps) {
  const m = useMessages();
  const { kinds } = useKinds({ client: p.client });
  const [own, setOwn] = useState<FeaturedWindow>(windows[0] ?? "week");
  const current: PopularWindow = window ?? own;
  const selected = (windows as readonly string[]).includes(current) ? current : "custom";
  if (!kinds?.signals || !windows.length) return null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="popular-grid">
      <Tabs
        value={selected}
        onValueChange={(value) => {
          const w = windows.find((x) => x === value);
          if (!w) return;
          if (window === undefined) setOwn(w);
          onWindowChange?.(w);
        }}
        className="gap-4"
      >
        <TabsList aria-label={m.t("listing.popularPeriod")} className="mx-auto flex h-auto w-full flex-wrap sm:w-fit">
          {windows.map((w) => (
            <TabsTrigger key={w} value={w} className="min-h-10 whitespace-normal px-3 py-2 text-center">
              {m.t(`listing.period_${w}` as MessageKey)}
            </TabsTrigger>
          ))}
        </TabsList>
        <TabsContent value={selected}>
          <PopularGrid {...p} query={query} window={current} />
        </TabsContent>
      </Tabs>
    </ContentKitUiRoot>
  );
}

function PopularGrid({ query, window, ...p }: CardListProps & { query: PopularQuery; window: PopularWindow }) {
  const m = useMessages();
  const q = useMemo(() => popularQuery(window, { ...query, summary: query.summary ?? true, limit: query.limit ?? p.pageSize, mode: cardListMode(p.locked, query.mode) }), [window, query, p.pageSize, p.locked]);
  const list = useContents(q, { client: p.client });
  const snapshot = list.popularity;
  const featured = (WINDOWS as readonly string[]).includes(window);
  return (
    <>
      {snapshot?.ready && !featured && <p className="mb-4 text-center text-muted-foreground">{m.t("listing.activityRange", { from: snapshot.from.slice(0, 10), until: lastIncludedDay(snapshot.until) })}</p>}
      <CardGrid {...p} surface={p.surface ?? "popular"} list={list} render={JSON.stringify(q)} filterKinds={query.kind ? [query.kind] : undefined} filterLanguage={query.display_language ?? query.language} empty={p.empty} infinite />
    </>
  );
}

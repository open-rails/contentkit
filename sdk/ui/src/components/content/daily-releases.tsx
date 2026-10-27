import { ArrowDown01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useContext, useId, useMemo, useState, useRef, type MouseEvent } from "react";
import type { DailyReleasesQuery } from "../../client/discovery.js";
import type { Card, ReleaseDay } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useDailyReleases, useExposure } from "../../react/discovery.js";
import { ContentKitContext } from "../../react/context.js";
import { useContentScope } from "../../react/use-resource.js";
import { useNearViewport } from "../../react/viewport.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
import { ContentSummaryTile } from "./summary-tile.js";
import { ContentFilterDialog } from "./filter-dialog.js";
import { useReleaseReveal } from "./release-reveal.js";
import { ContentCard } from "./card.js";
import { cardGridLayout, type CardListProps } from "./grid.js";

export interface DailyReleasesProps extends Omit<CardListProps, "infinite" | "pageSize" | "renderCard" | "loadMedia"> {
  /** One complete calendar window. Defaults to 10 days in the browser's time zone. */
  query?: DailyReleasesQuery;
  /** Cards initially revealed across the whole window, allocated newest first. Default 30. */
  initialVisible?: number;
  /** Collapse locked tier previews per day; expansion never grants access. Default false. */
  collapsePremium?: boolean;
  /** Host-specific nouns; defaults use localized generic item labels. */
  summaryLabels?: { premium?: (count: number) => string; filtered?: (count: number) => string };
  /** Additional cached cards revealed by a day's Show more button. Default 30. */
  revealStep?: number;
}

const count = (value: number, fallback: number) => Number.isFinite(value) ? Math.max(0, Math.floor(value)) : fallback;

/**
 * One metadata request for a calendar window, with no requests when a day is
 * opened or expanded. Only revealed, visible cards mount images and previews.
 */
export function DailyReleases({ query, initialVisible = 30, revealStep = 30, collapsePremium = false, summaryLabels, ...p }: DailyReleasesProps) {
  const m = useMessages();
  const { client, viewer } = useContentScope(p.client);
  const context = useContext(ContentKitContext);
  const request = useMemo(() => ({ days: 10, time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone, ...query }), [query]);
  const releases = useDailyReleases(request, { client: p.client });
  const layout = cardGridLayout(p.grid);
  const requestKey = JSON.stringify(request);
  // Guest preference changes create a fresh read scope. Keep the open day/dialog
  // while it loads, but never show the old cards or carry data across accounts.
  const identity = useMemo(() => ({ client, viewer, language: context?.language, accessRevision: context?.accessRevision, requestKey }), [client, viewer, context?.language, context?.accessRevision, requestKey]);
  const [previous, setPrevious] = useState({ identity, data: releases.data, revision: 0 });
  if (previous.identity !== identity || (releases.data && previous.data !== releases.data)) setPrevious({ identity, data: releases.data, revision: previous.revision + (previous.identity === identity ? 0 : 1) });
  const data = releases.data ?? (previous.identity === identity ? previous.data : undefined);
  const pending = !releases.data && Boolean(data);
  const days = useMemo(() => data?.days.filter((day) => (day.contents.length || day.filtered_count > 0)).sort((a, b) => b.date.localeCompare(a.date)) ?? [], [data]);
  let remaining = count(initialVisible, 30);
  return (
    <ContentKitUiRoot key={previous.revision} appearance={p.appearance} className={cn("grid gap-6 text-sm", p.className)} data-ckui="daily-releases" aria-busy={releases.loading}>
      {data && releases.error && <div role="alert" className="grid justify-items-start gap-2"><p className="text-destructive">{contentError(m, releases.error)}</p><Button type="button" variant="outline" onClick={releases.reload}>{m.t("common.retry")}</Button></div>}
      {releases.loading && !data ? <div className={layout.className} style={layout.style}>{Array.from({ length: 6 }, (_, i) => <Skeleton key={i} className={p.aspectRatio === undefined ? p.shape === "landscape" ? "aspect-video" : "aspect-[3/4]" : undefined} style={p.aspectRatio === undefined ? undefined : { aspectRatio: p.aspectRatio }} />)}</div> : releases.error && !data ? (
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, releases.error)}</p>
          <Button size="sm" variant="outline" onClick={releases.reload}>{m.t("common.retry")}</Button>
        </div>
      ) : !days.length ? <p role="status" className="text-muted-foreground">{p.empty ?? m.t("contents.empty")}</p> : days.map((day) => {
        const initial = Math.min(remaining, day.contents.length);
        remaining -= initial;
        return <ReleaseDaySection key={JSON.stringify([requestKey, data?.from_date, day.date])} day={day} pending={pending} collapsePremium={collapsePremium} summaryLabels={summaryLabels} language={request.display_language ?? request.language} kinds={request.kind ? [request.kind] : undefined} initial={initial} step={Math.max(1, count(revealStep, 30))} cardProps={p} />;
      })}
    </ContentKitUiRoot>
  );
}

type ReleaseCardProps = Omit<DailyReleasesProps, "query" | "initialVisible" | "revealStep" | "collapsePremium" | "summaryLabels">;
const lockedPremium = (card: Card) => !card.access.allowed && card.access.level === "tier";

function ReleaseDaySection({ day, initial, step, pending, cardProps: p, collapsePremium, summaryLabels, language, kinds }: {
  day: ReleaseDay; initial: number; step: number; pending: boolean; cardProps: ReleaseCardProps;
  collapsePremium: boolean; summaryLabels?: DailyReleasesProps["summaryLabels"]; language?: string; kinds?: string[];
}) {
  const m = useMessages();
  const id = useId();
  const [open, setOpen] = useState(initial > 0 || (!day.contents.length && day.filtered_count > 0));
  const [visible, setVisible] = useState(initial);
  const [premiumExpanded, setPremiumExpanded] = useState(false);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const filterTrigger = useRef<HTMLElement | null>(null);
  const dayHeading = useRef<HTMLButtonElement>(null);
  const cards = useMemo(() => day.contents.slice(0, visible), [day.contents, visible]);
  const premiumCount = useMemo(() => day.contents.filter(lockedPremium).length, [day.contents]);
  const summaryContentKind = kinds?.[0];
  const entries = useMemo(() => {
    let inserted = false;
    return cards.flatMap((card, i): { key: string; card: Card; summary: boolean; position: number }[] => {
      if (!collapsePremium || premiumExpanded || !lockedPremium(card)) return [{ key: card.id, card, summary: false, position: i + 1 }];
      if (inserted) return [];
      inserted = true;
      return [{ key: "premium-summary", card, summary: true, position: i + 1 }];
    });
  }, [cards, collapsePremium, premiumExpanded]);
  const exposed = useMemo(() => open && !pending ? entries.filter((entry) => !entry.summary).map((entry) => ({ ...entry.card, position: entry.position })) : [], [open, pending, entries]);
  const exposure = useExposure(exposed, { surface: p.surface ?? "releases", render: day.date, client: p.client });
  const layout = cardGridLayout(p.grid);
  const reveal = useReleaseReveal(`${visible}|${entries.map((entry) => entry.key).join("\0")}`);
  const label = new Intl.DateTimeFormat(m.language, { dateStyle: "full", timeZone: "UTC" }).format(new Date(`${day.date}T12:00:00Z`));
  const toggle = () => {
    if (!open && !visible) setVisible(Math.min(step, day.contents.length));
    setOpen(!open);
  };
  const expandPremium = (event: MouseEvent<HTMLButtonElement>) => {
    reveal.prepare(event.detail === 0);
    setPremiumExpanded(true);
  };
  return (
    <section data-ckui="release-day" data-date={day.date}>
      <h2>
        <Button ref={dayHeading} id={`${id}-heading`} type="button" variant="ghost" className="h-auto w-full justify-between gap-3 rounded-none border-x-0 border-t-0 border-border px-0 py-3 text-start text-lg font-semibold hover:bg-transparent aria-expanded:bg-transparent dark:hover:bg-transparent" aria-expanded={open} aria-controls={`${id}-contents`} onClick={toggle}>
          <time dateTime={day.date}>{label}</time>
          <HugeiconsIcon icon={ArrowDown01Icon} strokeWidth={2} className={cn("box-content size-5 shrink-0 rounded-full bg-muted p-1.5 text-muted-foreground transition-transform duration-200 group-hover/button:text-foreground motion-reduce:transition-none", !open && "-rotate-90")} aria-hidden />
        </Button>
      </h2>
      {open && pending && <Skeleton className="h-24 w-full" />}
      <div hidden={pending} style={pending ? { display: "none" } : undefined} id={`${id}-contents`} role="region" aria-labelledby={`${id}-heading`} aria-hidden={!open || pending} inert={!open || pending} className={cn("grid transition-[grid-template-rows,opacity] duration-250 ease-out motion-reduce:transition-none", open ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0")}>
        <div className="min-h-0 overflow-hidden">
          <ol ref={reveal.grid} className={cn(layout.className, "pt-2")} style={layout.style}>
            {entries.map((entry) => entry.summary ?
              <ContentSummaryTile key={entry.key} tileKey={entry.key} contentKind={kinds?.[0] ?? entry.card.kind} open={open && !pending} preview={entry.card} cardProps={p} kind="premium" label={summaryLabels?.premium?.(premiumCount) ?? m.plural("releases.premiumMore", premiumCount)} onClick={expandPremium} /> :
              <ReleaseCard key={entry.key} card={entry.card} open={open && !pending} position={entry.position} exposure={exposure} cardProps={p} />)}
            {day.filtered_count > 0 && <ContentSummaryTile tileKey="filtered-summary" contentKind={summaryContentKind} open={open && !pending} cardProps={p} kind="filtered" label={summaryLabels?.filtered?.(day.filtered_count) ?? m.plural("releases.filteredMore", day.filtered_count)} onClick={(event) => { filterTrigger.current = event.currentTarget; setFiltersOpen(true); }} />}
          </ol>
          {visible < day.contents.length && <div className="flex justify-center pt-5 pb-1"><Button type="button" variant="outline" onClick={(event) => { reveal.prepare(event.detail === 0); setVisible((n) => Math.min(n + step, day.contents.length)); }}>{m.t("contents.showMore")}</Button></div>}
        </div>
      </div>
      <ContentFilterDialog returnFocus={() => !pending && filterTrigger.current?.isConnected ? filterTrigger.current : dayHeading.current} open={filtersOpen} onOpenChange={setFiltersOpen} client={p.client} kinds={kinds} language={language} counts={day.filtered_entities ?? []} description={m.t("releases.filterContext", { date: label })} appearance={p.appearance} />
    </section>
  );
}


function ReleaseCard({ card, open, position, exposure, cardProps: p }: { card: Card; open: boolean; position: number; exposure: ReturnType<typeof useExposure>; cardProps: ReleaseCardProps }) {
  const [viewport, visible] = useNearViewport({ margin: "0px", leaveMargin: "1200px 0px" });
  const tracked = useMemo(() => exposure.track(card.id, position), [exposure, card.id, position]);
  const ref = useCallback((element: HTMLLIElement | null) => { viewport(element); tracked(element); }, [viewport, tracked]);
  return <li ref={ref} data-release-key={card.id}>
    <ContentCard card={card} client={p.client} href={() => exposure.href(card.id, position, p.href ? p.href(card) : card.path)} shape={p.shape} aspectRatio={p.aspectRatio} titlePosition={p.titlePosition} loadMedia={open && visible} entityKinds={p.entityKinds} chips={p.chips} chipOverflow={p.chipOverflow} chipFit={p.chipFit} filterEntity={p.filterEntity} entityStyle={p.entityStyle} entityLayout={p.entityLayout} metadata={p.metadata} showCounts={p.showCounts} tiers={p.tiers} labels={p.labels} showChannel={p.showChannel} channelHref={p.channelHref} decorate={p.decorate} cardTitle={p.cardTitle} onOpen={() => exposure.click(card.id, position, exposure.href(card.id, position, p.href ? p.href(card) : card.path))} />
  </li>;
}

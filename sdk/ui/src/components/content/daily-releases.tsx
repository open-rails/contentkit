import { ArrowDown01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useId, useMemo, useState } from "react";
import type { DailyReleasesQuery } from "../../client/discovery.js";
import type { Card, ReleaseDay } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useDailyReleases, useExposure } from "../../react/discovery.js";
import { useContentScope } from "../../react/use-resource.js";
import { useNearViewport } from "../../react/viewport.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
import { ContentCard } from "./card.js";
import { cardGridLayout, type CardListProps } from "./grid.js";

export interface DailyReleasesProps extends Omit<CardListProps, "infinite" | "pageSize" | "renderCard" | "loadMedia"> {
  /** One complete calendar window. Defaults to 10 days in the browser's time zone. */
  query?: DailyReleasesQuery;
  /** Cards initially revealed across the whole window, allocated newest first. Default 30. */
  initialVisible?: number;
  /** Additional cached cards revealed by a day's Show more button. Default 30. */
  revealStep?: number;
}

const count = (value: number, fallback: number) => Number.isFinite(value) ? Math.max(0, Math.floor(value)) : fallback;

/**
 * One metadata request for a calendar window, with no requests when a day is
 * opened or expanded. Only revealed, visible cards mount images and previews.
 */
export function DailyReleases({ query, initialVisible = 30, revealStep = 30, ...p }: DailyReleasesProps) {
  const m = useMessages();
  const { scope } = useContentScope(p.client);
  const request = useMemo(() => ({ days: 10, time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone, ...query }), [query]);
  const releases = useDailyReleases(request, { client: p.client });
  const layout = cardGridLayout(p.grid);
  const days = useMemo(() => releases.data?.days.filter((day) => day.contents.length).sort((a, b) => b.date.localeCompare(a.date)) ?? [], [releases.data]);
  let remaining = count(initialVisible, 30);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-6 text-sm", p.className)} data-ckui="daily-releases" aria-busy={releases.loading}>
      {releases.loading && !releases.data ? <div className={layout.className} style={layout.style}>{Array.from({ length: 6 }, (_, i) => <Skeleton key={i} className={p.aspectRatio === undefined ? p.shape === "landscape" ? "aspect-video" : "aspect-[3/4]" : undefined} style={p.aspectRatio === undefined ? undefined : { aspectRatio: p.aspectRatio }} />)}</div> : releases.error && !releases.data ? (
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, releases.error)}</p>
          <Button size="sm" variant="outline" onClick={releases.reload}>{m.t("common.retry")}</Button>
        </div>
      ) : !days.length ? <p role="status" className="text-muted-foreground">{p.empty ?? m.t("contents.empty")}</p> : days.map((day) => {
        const initial = Math.min(remaining, day.contents.length);
        remaining -= initial;
        return <ReleaseDaySection key={JSON.stringify([scope, request, releases.data?.from_date, day.date])} day={day} initial={initial} step={Math.max(1, count(revealStep, 30))} cardProps={p} />;
      })}
    </ContentKitUiRoot>
  );
}

function ReleaseDaySection({ day, initial, step, cardProps: p }: { day: ReleaseDay; initial: number; step: number; cardProps: Omit<DailyReleasesProps, "query" | "initialVisible" | "revealStep"> }) {
  const m = useMessages();
  const id = useId();
  const [open, setOpen] = useState(initial > 0);
  const [visible, setVisible] = useState(initial);
  const cards = useMemo(() => day.contents.slice(0, visible), [day.contents, visible]);
  const exposed = useMemo(() => open ? cards : [], [open, cards]);
  const exposure = useExposure(exposed, { surface: p.surface ?? "releases", render: day.date, client: p.client });
  const layout = cardGridLayout(p.grid);
  // The server already assigned the calendar day in the requested time zone.
  const label = new Intl.DateTimeFormat(m.language, { dateStyle: "full", timeZone: "UTC" }).format(new Date(`${day.date}T12:00:00Z`));
  const toggle = () => {
    if (!open && !visible) setVisible(Math.min(step, day.contents.length));
    setOpen(!open);
  };
  return (
    <section data-ckui="release-day" data-date={day.date}>
      <h2>
        <Button id={`${id}-heading`} type="button" variant="ghost" className="h-auto w-full justify-start gap-3 px-0 py-3 text-start text-lg font-semibold" aria-expanded={open} aria-controls={`${id}-contents`} onClick={toggle}>
          <HugeiconsIcon icon={ArrowDown01Icon} strokeWidth={2} className={cn("size-5 shrink-0 transition-transform duration-200 motion-reduce:transition-none", !open && "-rotate-90")} aria-hidden />
          <time dateTime={day.date}>{label}</time>
          <span className="ml-auto shrink-0 text-sm font-normal text-muted-foreground">{m.plural("contents.items", day.contents.length)}</span>
        </Button>
      </h2>
      <div id={`${id}-contents`} role="region" aria-labelledby={`${id}-heading`} aria-hidden={!open} inert={!open} className={cn("grid transition-[grid-template-rows,opacity] duration-250 ease-out motion-reduce:transition-none", open ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0")}>
        <div className="min-h-0 overflow-hidden">
          <ol className={cn(layout.className, "pt-2")} style={layout.style}>
            {cards.map((card, i) => <ReleaseCard key={card.id} card={card} open={open} position={i + 1} exposure={exposure} cardProps={p} />)}
          </ol>
          {visible < day.contents.length && <div className="flex justify-center pt-5 pb-1"><Button type="button" variant="outline" onClick={() => setVisible((n) => Math.min(n + step, day.contents.length))}>{m.t("contents.showMore")}</Button></div>}
        </div>
      </div>
    </section>
  );
}

function ReleaseCard({ card, open, position, exposure, cardProps: p }: { card: Card; open: boolean; position: number; exposure: ReturnType<typeof useExposure>; cardProps: Omit<DailyReleasesProps, "query" | "initialVisible" | "revealStep"> }) {
  const [viewport, visible] = useNearViewport({ margin: "0px" });
  const tracked = useMemo(() => exposure.track(card.id, position), [exposure, card.id, position]);
  const ref = useCallback((element: HTMLLIElement | null) => { viewport(element); tracked(element); }, [viewport, tracked]);
  return <li ref={ref}>
    <ContentCard card={card} href={p.href} shape={p.shape} aspectRatio={p.aspectRatio} titlePosition={p.titlePosition} loadMedia={open && visible} groupKinds={p.groupKinds} chips={p.chips} filterEntity={p.filterEntity} entityStyle={p.entityStyle} metadata={p.metadata} showCounts={p.showCounts} tiers={p.tiers} labels={p.labels} owner={p.owner} ownerHref={p.ownerHref} decorate={p.decorate} onOpen={() => exposure.click(card.id, position)} />
  </li>;
}

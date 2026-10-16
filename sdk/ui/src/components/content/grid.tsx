import { cn } from "cn";
import { useContext, useEffect, useMemo, useState, type CSSProperties, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentQuery, HistoryQuery, RailQuery, SearchQuery } from "../../client/discovery.js";
import type { Card } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext } from "../../react/context.js";
import { useContents, useExposure, useHistory, useLockedCount, useRecommendations, useSearch, useSimilar, type UseRail } from "../../react/discovery.js";
import { useKinds } from "../../react/groups.js";
import type { UseList } from "../../react/use-resource.js";
import { useNearViewport } from "../../react/viewport.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError, useAction } from "../social/parts.js";
import { ContentCard, type ContentCardProps } from "./card.js";

export interface CardListProps extends Omit<ContentCardProps, "card" | "onOpen" | "children" | "className"> {
  /** Where the list is, for exposures and click attribution: "home", "search", "similar". */
  surface?: string;
  /** Loads the next page as the end nears, instead of a button. */
  infinite?: boolean;
  /** A card's entry instead of ContentCard. */
  renderCard?: (card: Card, position: number) => ReactNode;
  /** Shown when nothing matches. */
  empty?: ReactNode;
  pageSize?: number;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
  /** Opt into a fluid grid, optionally with a desktop column ceiling. */
  grid?: CardGridLayout;
}

const GRID = "grid grid-cols-[repeat(auto-fill,minmax(min(100%,10rem),1fr))] gap-x-4 gap-y-6";

export interface CardGridLayout {
  /** Minimum desktop card width: CSS pixels or a CSS length such as clamp(...). Cards fill the available space. */
  minCardWidth: number | string;
  /** Maximum desktop columns. Omit to use all available width without a ceiling. */
  maxColumns?: number;
  /** Equal-width columns below the desktop breakpoint. Default 2. */
  mobileColumns?: number;
}

/** Shared by complete lists, release-day sections and grid-style previews. */
export function cardGridLayout(layout?: CardGridLayout): { className: string; style?: CSSProperties } {
  if (!layout) return { className: GRID };
  const width = typeof layout.minCardWidth === "string" ? layout.minCardWidth.trim() || "160px" : `${Number.isFinite(layout.minCardWidth) ? Math.max(1, layout.minCardWidth) : 160}px`;
  const columns = layout.mobileColumns !== undefined && Number.isFinite(layout.mobileColumns) ? Math.max(1, Math.floor(layout.mobileColumns)) : 2;
  const maximum = layout.maxColumns !== undefined && Number.isFinite(layout.maxColumns) ? Math.max(1, Math.floor(layout.maxColumns)) : undefined;
  const trackWidth = maximum === undefined ? width : `max(${width}, calc((100% - ${maximum - 1}rem) / ${maximum}))`;
  return {
    className: "grid grid-cols-[repeat(var(--ckui-card-mobile-columns),minmax(0,1fr))] gap-x-4 gap-y-6 md:grid-cols-[repeat(auto-fill,minmax(min(100%,var(--ckui-card-min-width)),1fr))]",
    style: { "--ckui-card-min-width": trackWidth, "--ckui-card-mobile-columns": columns } as CSSProperties,
  };
}

/** A list's cards in a responsive grid, paged, with exposures and click signals. */
export function CardGrid({ list, render, ...p }: CardListProps & { list: UseList<Card>; render: string }) {
  const m = useMessages();
  const { t } = m;
  const { kinds } = useKinds({ client: p.client });
  const exposure = useExposure(list.items, { surface: p.surface ?? "list", render, client: p.client });
  const [sentinel, near] = useNearViewport({ margin: "600px 0px", leaveMargin: "1200px 0px" });
  const { hasMore, loadingMore, loadMore, error } = list;
  useEffect(() => {
    if (p.infinite && near && hasMore && !loadingMore && !error) void loadMore();
  }, [p.infinite, near, hasMore, loadingMore, loadMore, error]);
  const tiers = p.tiers ?? kinds?.tiers;
  const layout = cardGridLayout(p.grid);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="content-grid" aria-busy={list.loading}>
      {list.loading && !list.items.length ? (
        <div className={layout.className} style={layout.style}>
          {Array.from({ length: 8 }, (_, i) => (
            <Skeleton key={i} className={p.aspectRatio === undefined ? p.shape === "landscape" ? "aspect-video w-full" : "aspect-[3/4] w-full" : "w-full"} style={p.aspectRatio === undefined ? undefined : { aspectRatio: p.aspectRatio }} />
          ))}
        </div>
      ) : list.error && !list.items.length ? (
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, list.error)}</p>
          <Button size="sm" variant="outline" onClick={list.reload}>
            {t("common.retry")}
          </Button>
        </div>
      ) : !list.items.length ? (
        <div className="text-muted-foreground" role="status">
          {p.empty ?? t("contents.empty")}
        </div>
      ) : (
        <ol className={layout.className} style={layout.style}>
          {list.items.map((card, i) => (
            <li key={card.id} ref={exposure.track(card.id, i + 1)}>
              {p.renderCard ? (
                p.renderCard(card, i + 1)
              ) : (
                <ContentCard
                  card={card}
                  client={p.client}
                  href={p.href}
                  shape={p.shape}
                  aspectRatio={p.aspectRatio}
                  titlePosition={p.titlePosition}
                  loadMedia={p.loadMedia}
                  groupKinds={p.groupKinds}
                  chips={p.chips}
                  chipOverflow={p.chipOverflow}
                  chipFit={p.chipFit}
                  filterEntity={p.filterEntity}
                  entityStyle={p.entityStyle}
                  entityLayout={p.entityLayout}
                  metadata={p.metadata}
                  showCounts={p.showCounts}
                  tiers={tiers}
                  labels={p.labels}
                  owner={p.owner}
                  ownerHref={p.ownerHref}
                  decorate={p.decorate}
                  onOpen={() => exposure.click(card.id, i + 1)}
                />
              )}
            </li>
          ))}
        </ol>
      )}
      {list.hasMore && (
        <div ref={sentinel} className="grid justify-items-center gap-2">
          {list.error && list.items.length > 0 && <p role="alert" className="text-destructive">{contentError(m, list.error)}</p>}
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("contents.loading") : list.error ? t("common.retry") : t("contents.loadMore")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

/**
 * What a list does with cards the reader doesn't open: shows them marked
 * (show), leaves them out (hide), or lists them apart, counted, behind a
 * toggle that loads them when opened (collapse).
 */
export type LockedCards = "show" | "hide" | "collapse";

const LOCKED_LEVELS = ["tier", "group", "ppv", "group_ppv"] as const;

/** Contents as cards (GET /v1/contents): any sort and filter, paged. */
export function ContentGrid({ query, locked = "show", ...p }: CardListProps & { query: ContentQuery; locked?: LockedCards }) {
  const q = useMemo<ContentQuery>(
    () => ({ ...query, limit: query.limit ?? p.pageSize, ...(locked === "show" ? {} : { mode: "hide" }) }),
    [query, p.pageSize, locked],
  );
  const list = useContents(q, { client: p.client });
  return (
    <>
      {locked === "collapse" && <LockedSection {...p} query={query} />}
      <CardGrid {...p} list={list} render={JSON.stringify(q)} />
    </>
  );
}

/** A list's locked cards, counted, loaded and shown on opening. */
function LockedSection({ query, ...p }: CardListProps & { query: ContentQuery }) {
  const m = useMessages();
  const [open, setOpen] = useState(false);
  const { count } = useLockedCount(query, { client: p.client });
  const q = useMemo<ContentQuery | null>(
    () => (open ? { ...query, mode: "lock", access: query.access ?? LOCKED_LEVELS, limit: query.limit ?? p.pageSize } : null),
    [open, query, p.pageSize],
  );
  const list = useContents(q, { client: p.client });
  const items = useMemo(() => list.items.filter((c) => !c.access.allowed), [list.items]);
  if (!count) return null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className="grid gap-3 text-sm" data-ckui="locked-cards">
      <Button variant="outline" size="sm" className="justify-self-start" aria-expanded={open} onClick={() => setOpen(!open)}>
        {open ? m.t("contents.lockedHide") : m.plural("contents.lockedShow", count)}
      </Button>
      {open && <CardGrid {...p} list={{ ...list, items }} render={JSON.stringify(q)} />}
    </ContentKitUiRoot>
  );
}

/** Search results as cards; nothing until the query has text. */
export function SearchResults({ query, ...p }: CardListProps & { query: SearchQuery }) {
  const { t } = useMessages();
  const q = useMemo(() => ({ ...query, limit: query.limit ?? p.pageSize }), [query, p.pageSize]);
  const list = useSearch(q, { client: p.client });
  if (!query.q.trim()) return null;
  return <CardGrid surface="search" empty={t("contents.noResults", { q: query.q.trim() })} {...p} list={list} render={JSON.stringify(q)} />;
}

/** The signed-in reader's history as cards, each removable, with a clear-all. */
export function ContentHistory({ query = {}, ...p }: CardListProps & { query?: HistoryQuery }) {
  const { t } = useMessages();
  const ctx = useContext(ContentKitContext);
  const q = useMemo(() => ({ ...query, limit: query.limit ?? p.pageSize }), [query, p.pageSize]);
  const list = useHistory(q, { client: p.client });
  const forget = useAction("history.forget");
  if (ctx?.viewer === null) return <p className="text-muted-foreground">{t("contents.historySignedOut")}</p>;
  return (
    <div className="grid gap-3" data-ckui="content-history">
      {list.items.length > 0 && (
        <Button size="sm" variant="outline" className="justify-self-end" disabled={forget.pending} onClick={() => void forget.run(() => list.forget(query.kind ? { kind: query.kind } : {}))}>
          {t("contents.clearHistory")}
        </Button>
      )}
      <CardGrid
        surface="history"
        empty={t("contents.historyEmpty")}
        {...p}
        list={list}
        render={JSON.stringify(q)}
        renderCard={
          p.renderCard ??
          ((card) => (
            <ContentCard
              card={card}
              client={p.client}
              href={p.href}
              shape={p.shape}
              aspectRatio={p.aspectRatio}
              titlePosition={p.titlePosition}
              loadMedia={p.loadMedia}
              groupKinds={p.groupKinds}
              chips={p.chips}
              chipOverflow={p.chipOverflow}
              chipFit={p.chipFit}
              filterEntity={p.filterEntity}
              entityStyle={p.entityStyle}
              entityLayout={p.entityLayout}
              metadata={p.metadata}
              showCounts={p.showCounts}
              tiers={p.tiers}
              labels={p.labels}
              owner={p.owner}
              ownerHref={p.ownerHref}
              decorate={p.decorate}
            >
              <Button size="xs" variant="ghost" className="justify-self-start" disabled={forget.pending} onClick={() => void forget.run(() => list.forget({ content_id: card.id }))}>
                {t("contents.removeFromHistory")}
              </Button>
            </ContentCard>
          ))
        }
      />
    </div>
  );
}

export interface ContentRailProps extends Omit<CardListProps, "infinite" | "empty"> {
  /** The rail's heading. */
  title: ReactNode;
  /** Contents like this one. */
  similar?: string;
  /** For the signed-in reader. */
  recommendations?: boolean;
  /** The reader's history: { status: "in_progress" } is a continue-watching rail. */
  history?: HistoryQuery;
  /** An ordered group's members in its order: a featured rail (its windows apply). */
  group?: string;
  /** Any list: a sort and filters. */
  list?: ContentQuery;
  /** Filters and reader options of a similar or recommended rail. */
  query?: RailQuery;
  /** A link beside the heading, e.g. to the whole list. */
  more?: ReactNode;
}

/**
 * A horizontal row of contents: similar, recommended, the reader's history,
 * an ordered group's members or any list. It hides itself when it has none or
 * the site runs without what it needs.
 */
export function ContentRail({ title, similar, recommendations, history, group, list: listQuery, query = {}, more, ...p }: ContentRailProps) {
  const limit = query.limit ?? p.pageSize ?? 12;
  const q = useMemo(() => ({ ...query, limit }), [query, limit]);
  const listing = useMemo(
    () => (listQuery ? { ...listQuery, limit: listQuery.limit ?? limit } : group ? { groups: [group], sort: "position" as const, limit } : null),
    [listQuery, group, limit],
  );
  const like = useSimilar(recommendations || history || listing ? null : similar, q, { client: p.client });
  const mine = useRecommendations(recommendations ? q : null, { client: p.client });
  const seen = useHistory(history ? { ...history, limit: history.limit ?? limit } : null, { client: p.client });
  const listed = useContents(listing, { client: p.client });
  const rail: UseRail = recommendations
    ? mine
    : history || listing
      ? (() => {
          const l = history ? seen : listed;
          return { items: l.items, loading: l.loading, error: l.error, unavailable: l.error?.code === "not_configured", reload: l.reload };
        })()
      : like;
  const { kinds } = useKinds({ client: p.client });
  const surface = p.surface ?? (recommendations ? "recommendations" : history ? "history" : group ? "featured" : listing ? "list" : "similar");
  const exposure = useExposure(rail.items, { surface, render: JSON.stringify([similar, recommendations, history, group, listing, q]), client: p.client });
  const source = similar || recommendations || history || group || listing;
  const layout = p.grid ? cardGridLayout(p.grid) : undefined;
  if (!source || rail.unavailable || (!rail.loading && !rail.items.length)) return null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="content-rail" data-surface={surface}>
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-base font-semibold">{title}</h2>
        {more}
      </div>
      <ol className={layout?.className ?? "relative flex snap-x gap-4 overflow-x-auto pb-2"} style={layout?.style}>
        {(rail.loading && !rail.items.length ? Array.from({ length: 6 }, () => null) : rail.items).map((card, i) => (
          <li key={card?.id ?? i} className={layout ? undefined : "w-36 shrink-0 snap-start sm:w-44"} ref={card ? exposure.track(card.id, i + 1) : undefined}>
            {!card ? (
              <Skeleton className={p.aspectRatio === undefined ? p.shape === "landscape" ? "aspect-video w-full" : "aspect-[3/4] w-full" : "w-full"} style={p.aspectRatio === undefined ? undefined : { aspectRatio: p.aspectRatio }} />
            ) : p.renderCard ? (
              p.renderCard(card, i + 1)
            ) : (
              <ContentCard
                card={card}
                client={p.client}
                href={p.href}
                shape={p.shape}
                aspectRatio={p.aspectRatio}
                titlePosition={p.titlePosition}
                loadMedia={p.loadMedia}
                groupKinds={p.groupKinds}
                chips={p.chips}
                chipOverflow={p.chipOverflow}
                chipFit={p.chipFit}
                filterEntity={p.filterEntity}
                entityStyle={p.entityStyle}
                entityLayout={p.entityLayout}
                metadata={p.metadata}
                showCounts={p.showCounts}
                tiers={p.tiers ?? kinds?.tiers}
                labels={p.labels}
                owner={p.owner}
                ownerHref={p.ownerHref}
                decorate={p.decorate}
                onOpen={() => exposure.click(card.id, i + 1)}
              />
            )}
          </li>
        ))}
      </ol>
    </ContentKitUiRoot>
  );
}

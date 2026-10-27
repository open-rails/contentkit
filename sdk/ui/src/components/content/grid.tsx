import { cn } from "cn";
import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { AccessMode, ContentQuery, HistoryQuery, RailQuery, SearchQuery } from "../../client/discovery.js";
import type { Card, EntityCard, ListingSummary } from "../../client/generated/wire.js";
import { EntityLink, type EntityLike } from "../entities/parts.js";
import { useMessages } from "../../i18n/context.js";
import { useContents, useExposure, useHistory, useRecommendations, useSearch, useSimilar, type UseRail } from "../../react/discovery.js";
import { useKinds } from "../../react/entities.js";
import type { UseList } from "../../react/use-resource.js";
import { ContentKitContext } from "../../react/context.js";
import { useContentScope } from "../../react/use-resource.js";
import { useNearViewport } from "../../react/viewport.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
import { ContentFilterDialog } from "./filter-dialog.js";
import { useReleaseReveal } from "./release-reveal.js";
import { ContentSummaryTile } from "./summary-tile.js";
import { ContentCard, type ContentCardProps } from "./card.js";
import { PagedContentGrid, PagedSearchResults } from "./paged-grid.js";
import { eventId } from "../../client/beacons.js";

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
  /** Collapse paid/locked cards into a counted tile, or hide them; default show. */
  locked?: LockedCards;
  /** Optional-filter summary tiles are on by default; hosts may suppress them in previews. */
  showFilteredSummary?: boolean;
  /** Show the collapsed locked tile only at this measured grid width. Default 0. */
  lockedSummaryMinColumns?: number;
  /** Limit rendered preview tiles without changing the matching selection or summary counts. */
  cardLimit?: number;
  /** Limit cards in mobile previews without mounting their images. */
  mobileCardLimit?: number;
  /** Host-specific nouns for summary controls. */
  summaryLabels?: { premium?: (count: number) => string; filtered?: (count: number) => string };
  /** Opt into a fluid grid, optionally with a desktop column ceiling. */
  grid?: CardGridLayout;
  /** Numbered pages with a shown range instead of load more or infinite scroll. */
  pagination?: "pages";
  /** The numbered page, from 1; with onPageChange the host keeps it (in its URL). */
  page?: number;
  onPageChange?: (page: number) => void;
  /** Each numbered page's link, for crawlers and opening in a new tab. */
  pageHref?: (page: number) => string;
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
  const width =
    typeof layout.minCardWidth === "string" ? layout.minCardWidth.trim() || "160px" : `${Number.isFinite(layout.minCardWidth) ? Math.max(1, layout.minCardWidth) : 160}px`;
  const columns = layout.mobileColumns !== undefined && Number.isFinite(layout.mobileColumns) ? Math.max(1, Math.floor(layout.mobileColumns)) : 2;
  const maximum = layout.maxColumns !== undefined && Number.isFinite(layout.maxColumns) ? Math.max(1, Math.floor(layout.maxColumns)) : undefined;
  const trackWidth = maximum === undefined ? width : `max(${width}, calc((100% - ${maximum - 1}rem) / ${maximum}))`;
  return {
    className:
      "grid grid-cols-[repeat(var(--ckui-card-mobile-columns),minmax(0,1fr))] gap-x-4 gap-y-6 md:grid-cols-[repeat(auto-fill,minmax(min(100%,var(--ckui-card-min-width)),1fr))]",
    style: { "--ckui-card-min-width": trackWidth, "--ckui-card-mobile-columns": columns } as CSSProperties,
  };
}

/** A list carries exact selection-wide counts when the API was asked for them. */
type SummaryList = UseList<Card> & { summary?: ListingSummary };
type GridProps = CardListProps & { list: SummaryList; render: string; filterKinds?: readonly string[]; filterLanguage?: string; horizontal?: boolean; searchQuery?: string; queryID?: string; ranker?: string; positionOffset?: number; settledEmpty?: boolean };

/** A list's cards and summary controls share one responsive, animated grid. */
export function CardGrid(p: GridProps) {
  const { client, viewer, language } = useContentScope(p.client);
  const context = useContext(ContentKitContext);
  const [boundary, setBoundary] = useState({ client, viewer, language, access: context?.accessRevision, render: p.render, key: 0 });
  let session = boundary;
  if (boundary.client !== client || boundary.viewer !== viewer || boundary.language !== language || boundary.access !== context?.accessRevision || boundary.render !== p.render) {
    session = { client, viewer, language, access: context?.accessRevision, render: p.render, key: boundary.key + 1 };
    setBoundary(session);
  }
  return <CardGridBody key={session.key} {...p} />;
}

function CardGridBody({ list, render, filterKinds, filterLanguage, horizontal, searchQuery, queryID, ranker, positionOffset = 0, settledEmpty, ...p }: GridProps) {
  const m = useMessages();
  const { t } = m;
  const { kinds } = useKinds({ client: p.client });
  const [expanded, setExpanded] = useState(false);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const filterTrigger = useRef<HTMLElement | null>(null);
  const [element, setElement] = useState<HTMLOListElement | null>(null);
  const [columns, setColumns] = useState(0);
  const [mobile, setMobile] = useState(() => globalThis.matchMedia?.("(max-width: 767px)").matches ?? false);
  useLayoutEffect(() => {
    if (!element) return;
    const measure = () => {
      const tracks = getComputedStyle(element).gridTemplateColumns.trim();
      setColumns(tracks && tracks !== "none" ? tracks.split(/\s+/).length : 0);
      setMobile(globalThis.matchMedia?.("(max-width: 767px)").matches ?? false);
    };
    measure();
    const observer = typeof ResizeObserver === "function" ? new ResizeObserver(measure) : undefined;
    observer?.observe(element);
    addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      removeEventListener("resize", measure);
    };
  }, [element]);
  const lockedCount = list.summary?.locked_count ?? 0;
  const filteredCount = p.showFilteredSummary === false ? 0 : (list.summary?.filtered_count ?? 0);
  const collapse = p.locked === "collapse" && !expanded && lockedCount > 0;
  const lockedTile = collapse && lockedCount > 0 && columns >= Math.max(0, p.lockedSummaryMinColumns ?? 0);
  const entries = useMemo(() => {
    const result: ({ key: string; card: Card; position: number } | { key: string; premium: true; card?: Card })[] = [];
    let inserted = false;
    for (const [i, card] of list.items.entries()) {
      if (isLockedCard(card) && (collapse || p.locked === "hide")) {
        if (lockedTile && !inserted) {
          result.push({ key: "premium-summary", premium: true, card });
          inserted = true;
        }
        continue;
      }
      result.push({ key: card.id, card, position: positionOffset + i + 1 });
    }
    if (lockedTile && !inserted) result.push({ key: "premium-summary", premium: true });
    const limit = mobile && p.mobileCardLimit !== undefined ? p.mobileCardLimit : p.cardLimit;
    return limit !== undefined && Number.isFinite(limit) ? result.slice(0, Math.max(0, Math.floor(limit))) : result;
  }, [list.items, collapse, p.locked, lockedTile, mobile, p.mobileCardLimit, p.cardLimit, positionOffset]);
  const shown = useMemo(() => entries.flatMap((entry) => ("premium" in entry ? [] : [{ ...entry.card, position: entry.position }])), [entries]);
  const exposure = useExposure(shown, { surface: p.surface ?? "list", render, client: p.client, query_id: queryID, search_query: searchQuery, ranker, settledEmpty });
  const [sentinel, near] = useNearViewport({ margin: "600px 0px", leaveMargin: "1200px 0px" });
  const { hasMore, loadingMore, loadMore, error } = list;
  useEffect(() => {
    if (p.infinite && near && hasMore && !loadingMore && !error) void loadMore();
  }, [p.infinite, near, hasMore, loadingMore, loadMore, error]);
  const { grid: animationGrid, prepare } = useReleaseReveal(entries.map((entry) => entry.key).join("\0"));
  const grid = useCallback(
    (node: HTMLOListElement | null) => {
      animationGrid.current = node;
      setElement(node);
    },
    [animationGrid],
  );
  const layout: ReturnType<typeof cardGridLayout> = horizontal
    ? { className: "relative flex snap-x gap-4 overflow-x-auto pb-2 [&>li]:w-36 [&>li]:shrink-0 [&>li]:snap-start sm:[&>li]:w-44" }
    : cardGridLayout(p.grid);
  const lowerBound = list.summary?.truncated ? "≥ " : "";
  const premiumLabel = lowerBound + (p.summaryLabels?.premium?.(lockedCount) ?? m.plural("contents.lockedShow", lockedCount));
  const filteredLabel = lowerBound + (p.summaryLabels?.filtered?.(filteredCount) ?? m.plural("releases.filteredMore", filteredCount));
  const showGrid = entries.length > 0 || filteredCount > 0 || (collapse && lockedCount > 0);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="content-grid" aria-busy={list.loading}>
      {list.loading && !list.items.length ? (
        <ol aria-hidden className={layout.className} style={layout.style}>
          {Array.from({ length: 8 }, (_, i) => (
            // The card's own shape: cover, then its title and entity rows, so loading does not jump.
            <li key={i} className="grid content-start gap-2">
              <Skeleton
                className={p.aspectRatio === undefined ? (p.shape === "landscape" ? "aspect-video w-full" : "aspect-[3/4] w-full") : "w-full"}
                style={p.aspectRatio === undefined ? undefined : { aspectRatio: p.aspectRatio }}
              />
              {p.titlePosition !== "cover" && (
                <div className="grid gap-1">
                  <div className="grid h-[2lh] content-center gap-2 text-lg leading-snug sm:text-xl">
                    <Skeleton className="h-4 w-full" />
                    <Skeleton className="h-4 w-3/4" />
                  </div>
                  {(p.entityLayout === "rows" ? (p.entityKinds ?? []) : (p.entityKinds ?? []).slice(0, 1)).map((kind) => (
                    <div key={kind} className="flex h-5 items-center">
                      <Skeleton className="h-3 w-2/3" />
                    </div>
                  ))}
                </div>
              )}
            </li>
          ))}
        </ol>
      ) : list.error && !list.items.length ? (
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, list.error)}</p>
          <Button size="sm" variant="outline" onClick={list.reload}>
            {t("common.retry")}
          </Button>
        </div>
      ) : !showGrid ? (
        <div className="text-muted-foreground" role="status">
          {p.empty ?? t("contents.empty")}
        </div>
      ) : (
        <ol ref={grid} tabIndex={-1} className={layout.className} style={layout.style}>
          {entries.map((entry) =>
            "premium" in entry ? (
              <ContentSummaryTile
                key={entry.key}
                tileKey={entry.key}
                kind="premium"
                contentKind={filterKinds?.[0] ?? entry.card?.kind}
                preview={entry.card}
                open
                cardProps={p}
                label={premiumLabel}
                onClick={(event) => {
                  prepare(event.detail === 0);
                  setExpanded(true);
                }}
              />
            ) : (
              <li key={entry.key} data-release-key={entry.key} ref={exposure.track(entry.card.id, entry.position)}>
                {p.renderCard ? (
                  p.renderCard(entry.card, entry.position)
                ) : (
                  <ContentCard
                    card={entry.card}
                    client={p.client}
                    href={(card) => exposure.href(card.id, entry.position, p.href ? p.href(card) : card.path)}
                    shape={p.shape}
                    aspectRatio={p.aspectRatio}
                    titlePosition={p.titlePosition}
                    loadMedia={p.loadMedia}
                    entityKinds={p.entityKinds}
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
                    showChannel={p.showChannel}
                    channelHref={p.channelHref}
                    decorate={p.decorate}
                    cardTitle={p.cardTitle}
                    onOpen={() => exposure.click(entry.card.id, entry.position, exposure.href(entry.card.id, entry.position, p.href ? p.href(entry.card) : entry.card.path))}
                  />
                )}
              </li>
            ),
          )}
          {filteredCount > 0 && (
            <ContentSummaryTile
              tileKey="filtered-summary"
              kind="filtered"
              contentKind={filterKinds?.[0] ?? list.items[0]?.kind}
              open
              cardProps={p}
              label={filteredLabel}
              onClick={(event) => {
                filterTrigger.current = event.currentTarget;
                setFiltersOpen(true);
              }}
            />
          )}
        </ol>
      )}
      {list.hasMore && (
        <div ref={sentinel} className="grid justify-items-center gap-2">
          {list.error && list.items.length > 0 && (
            <p role="alert" className="text-destructive">
              {contentError(m, list.error)}
            </p>
          )}
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("contents.loading") : list.error ? t("common.retry") : t("contents.loadMore")}
          </Button>
        </div>
      )}
      <ContentFilterDialog
        open={filtersOpen}
        onOpenChange={setFiltersOpen}
        returnFocus={() => (filterTrigger.current?.isConnected ? filterTrigger.current : element)}
        client={p.client}
        kinds={filterKinds}
        language={filterLanguage}
        counts={list.summary?.filtered_entities}
        countsLowerBound={list.summary?.truncated}
        appearance={p.appearance}
      />
    </ContentKitUiRoot>
  );
}

/**
 * What a list does with cards the reader doesn't open: shows them marked
 * (show), leaves them out (hide), or replaces them with a counted tile
 * that reveals the cached previews in place (collapse).
 */
export type LockedCards = "show" | "hide" | "collapse";

/** Settle locked-card admission before pagination, retaining explicit reader modes otherwise. */
export function cardListMode(locked: LockedCards | undefined, mode?: AccessMode): AccessMode | undefined {
  return locked === "hide" ? "hide" : locked === "collapse" ? "lock" : mode;
}

const LOCKED_LEVELS: readonly string[] = ["tier", "group", "ppv", "group_ppv"];
const isLockedCard = (card: Card) => !card.access.allowed && !card.access.unknown && LOCKED_LEVELS.includes(card.access.level);

/** Contents as cards (GET /v1/contents): any sort and filter, paged by load more, infinite scroll or numbered pages. */
export function ContentGrid(p: CardListProps & { query: ContentQuery }) {
  return p.pagination === "pages" ? <PagedContentGrid {...p} /> : <CursorContentGrid {...p} />;
}

function CursorContentGrid({ query, locked = "show", ...p }: CardListProps & { query: ContentQuery }) {
  const q = useMemo<ContentQuery>(
    () => ({
      ...query,
      summary: query.summary ?? true,
      limit: query.limit ?? p.pageSize,
      mode: cardListMode(locked, query.mode),
    }),
    [query, p.pageSize, locked],
  );
  const list = useContents(q, { client: p.client });
  return (
    <CardGrid
      {...p}
      locked={locked}
      list={list}
      render={JSON.stringify(q)}
      filterKinds={query.kind ? [query.kind] : undefined}
      filterLanguage={query.display_language ?? query.language}
    />
  );
}

export interface SearchResultsProps extends CardListProps {
  query: SearchQuery;
  /** An entity's page link; its canonical path by default. */
  entityHref?: (entity: EntityLike) => string | undefined;
}

/** Search results as cards, after links to the entities matching the query; nothing until the query has text. */
export function SearchResults(p: SearchResultsProps) {
  return p.pagination === "pages" ? <PagedSearchResults {...p} /> : <CursorSearchResults {...p} />;
}

function CursorSearchResults({ query, entityHref, ...p }: SearchResultsProps) {
  const { t } = useMessages();
  const q = useMemo(
    () => ({ ...query, mode: cardListMode(p.locked, query.mode), summary: query.summary ?? true, limit: query.limit ?? p.pageSize }),
    [query, p.pageSize, p.locked],
  );
  const list = useSearch(q, { client: p.client });
  const queryID = useMemo(() => eventId(), [JSON.stringify(query)]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!query.q.trim()) return null;
  return (
    <>
      <SearchEntities entities={list.entities} href={entityHref} />
      <CardGrid
        surface="search"
        empty={t("contents.noResults", { q: query.q.trim() })}
        {...p}
        list={list}
        render={JSON.stringify(q)}
        queryID={queryID}
        searchQuery={query.q}
        settledEmpty={!list.loading && !list.error && list.items.length === 0}
        ranker={query.sort ?? "relevance"}
        filterKinds={query.kind ? [query.kind] : undefined}
        filterLanguage={query.display_language ?? query.language}
      />
    </>
  );
}

/** Links to the entities whose names match a search, e.g. "Search the tag". */
export function SearchEntities({ entities, href }: { entities: readonly EntityCard[]; href?: (entity: EntityLike) => string | undefined }) {
  const { t } = useMessages();
  if (!entities.length) return null;
  return (
    <nav aria-label={t("contents.matchingEntities")} className="mb-4 flex flex-wrap gap-2" data-ckui="search-entities">
      {entities.map((entity) => (
        <EntityLink key={entity.id} entity={entity} href={href} className="inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-sm hover:bg-muted">
          <span>{entity.name || entity.slug}</span>
          <span className="text-muted-foreground tabular-nums">{entity.count}</span>
        </EntityLink>
      ))}
    </nav>
  );
}

export { ContentHistory } from "./history.js";

export interface ContentRailProps extends Omit<CardListProps, "infinite" | "empty"> {
  /** The rail's heading. */
  title: ReactNode;
  /** Contents like this one. */
  similar?: string;
  /** For the signed-in reader. */
  recommendations?: boolean;
  /** The reader's history: { status: "in_progress" } is a continue-watching rail. */
  history?: HistoryQuery;
  /** An ordered group's members in its order: a featured rail. */
  entity?: string;
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
export function ContentRail({ title, similar, recommendations, history, entity, list: listQuery, query = {}, more, ...p }: ContentRailProps) {
  const limit = query.limit ?? p.pageSize ?? 12;
  const q = useMemo(() => ({ ...query, mode: cardListMode(p.locked, query.mode), summary: query.summary ?? true, limit }), [query, limit, p.locked]);
  const listing = useMemo(
    () =>
      listQuery
        ? { ...listQuery, mode: cardListMode(p.locked, listQuery.mode), summary: listQuery.summary ?? true, limit: listQuery.limit ?? limit }
        : entity
          ? { groups: [entity], sort: "position" as const, mode: cardListMode(p.locked), summary: true, limit }
          : null,
    [listQuery, entity, limit, p.locked],
  );
  const like = useSimilar(recommendations || history || listing ? null : similar, q, { client: p.client });
  const mine = useRecommendations(recommendations ? q : null, { client: p.client });
  const seen = useHistory(history ? { ...history, mode: cardListMode(p.locked, history.mode), summary: history.summary ?? true, limit: history.limit ?? limit } : null, {
    client: p.client,
  });
  const listed = useContents(listing, { client: p.client });
  const rail: UseRail = recommendations
    ? mine
    : history || listing
      ? (() => {
          const l = history ? seen : listed;
          return { items: l.items, summary: l.summary, loading: l.loading, error: l.error, unavailable: l.error?.code === "not_configured", reload: l.reload };
        })()
      : like;
  const surface = p.surface ?? (recommendations ? "recommendations" : history ? "history" : entity ? "featured" : listing ? "list" : "similar");
  const source = similar || recommendations || history || entity || listing;
  if (!source || rail.unavailable || (!rail.loading && !rail.items.length && !rail.summary?.filtered_count && !rail.summary?.locked_count)) return null;
  const list: SummaryList = { ...rail, loadingMore: false, hasMore: false, loadMore: async () => {} };
  const kind = listQuery?.kind ?? history?.kind ?? q.kind;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="content-rail" data-surface={surface}>
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-base font-semibold">{title}</h2>
        {more}
      </div>
      <CardGrid
        {...p}
        className={undefined}
        surface={surface}
        horizontal={!p.grid}
        list={list}
        render={JSON.stringify([similar, recommendations, history, entity, listing, q])}
        filterKinds={kind ? [kind] : undefined}
        filterLanguage={listQuery?.display_language ?? history?.display_language ?? q.display_language ?? listQuery?.language ?? history?.language ?? q.language}
      />
    </ContentKitUiRoot>
  );
}

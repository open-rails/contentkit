import { cn } from "cn";
import { useContext, useEffect, useMemo, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentQuery, HistoryQuery, RailQuery, SearchQuery } from "../../client/discovery.js";
import type { Card } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext } from "../../react/context.js";
import { useContents, useExposure, useHistory, useRecommendations, useSearch, useSimilar, type UseRail } from "../../react/discovery.js";
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
}

const GRID = "grid grid-cols-[repeat(auto-fill,minmax(min(100%,10rem),1fr))] gap-x-4 gap-y-6";

/** A list's cards in a responsive grid, paged, with exposures and click signals. */
export function CardGrid({ list, render, ...p }: CardListProps & { list: UseList<Card>; render: string }) {
  const m = useMessages();
  const { t } = m;
  const { kinds } = useKinds({ client: p.client });
  const exposure = useExposure(list.items, { surface: p.surface ?? "list", render, client: p.client });
  const [sentinel, near] = useNearViewport({ margin: "600px 0px", leaveMargin: "1200px 0px" });
  const { hasMore, loadingMore, loadMore } = list;
  useEffect(() => {
    if (p.infinite && near && hasMore && !loadingMore) void loadMore();
  }, [p.infinite, near, hasMore, loadingMore, loadMore]);
  const tiers = p.tiers ?? kinds?.tiers;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="content-grid" aria-busy={list.loading}>
      {list.loading && !list.items.length ? (
        <div className={GRID}>
          {Array.from({ length: 8 }, (_, i) => (
            <Skeleton key={i} className={p.shape === "landscape" ? "aspect-video w-full" : "aspect-[3/4] w-full"} />
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
        <ol className={GRID}>
          {list.items.map((card, i) => (
            <li key={card.id} ref={exposure.track(card.id, i + 1)}>
              {p.renderCard ? (
                p.renderCard(card, i + 1)
              ) : (
                <ContentCard
                  card={card}
                  href={p.href}
                  shape={p.shape}
                  groupKinds={p.groupKinds}
                  chips={p.chips}
                  tiers={tiers}
                  labels={p.labels}
                  onOpen={() => exposure.click(card.id, i + 1)}
                />
              )}
            </li>
          ))}
        </ol>
      )}
      {list.hasMore && (
        <div ref={sentinel} className="grid justify-items-center">
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("contents.loading") : t("contents.loadMore")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

/** Contents as cards (GET /v1/contents): any sort and filter, paged. */
export function ContentGrid({ query, ...p }: CardListProps & { query: ContentQuery }) {
  const q = useMemo(() => ({ ...query, limit: query.limit ?? p.pageSize }), [query, p.pageSize]);
  const list = useContents(q, { client: p.client });
  return <CardGrid {...p} list={list} render={JSON.stringify(q)} />;
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
            <ContentCard card={card} href={p.href} shape={p.shape} groupKinds={p.groupKinds} chips={p.chips} tiers={p.tiers} labels={p.labels}>
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
  /** Filters and reader options of either source. */
  query?: RailQuery;
}

/** A horizontal row of similar or recommended contents; hidden when it has none or the site runs without them. */
export function ContentRail({ title, similar, recommendations, query = {}, ...p }: ContentRailProps) {
  const q = useMemo(() => ({ ...query, limit: query.limit ?? p.pageSize }), [query, p.pageSize]);
  const like = useSimilar(recommendations ? null : similar, q, { client: p.client });
  const mine = useRecommendations(recommendations ? q : null, { client: p.client });
  const rail: UseRail = recommendations ? mine : like;
  const { kinds } = useKinds({ client: p.client });
  const exposure = useExposure(rail.items, { surface: p.surface ?? (recommendations ? "recommendations" : "similar"), render: JSON.stringify([similar, q]), client: p.client });
  if (rail.unavailable || (!rail.loading && !rail.items.length) || (!recommendations && !similar)) return null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="content-rail">
      <h2 className="text-base font-semibold">{title}</h2>
      <ol className="flex snap-x gap-4 overflow-x-auto pb-2">
        {(rail.loading && !rail.items.length ? Array.from({ length: 6 }, () => null) : rail.items).map((card, i) => (
          <li key={card?.id ?? i} className="w-36 shrink-0 snap-start sm:w-44" ref={card ? exposure.track(card.id, i + 1) : undefined}>
            {!card ? (
              <Skeleton className={p.shape === "landscape" ? "aspect-video w-full" : "aspect-[3/4] w-full"} />
            ) : p.renderCard ? (
              p.renderCard(card, i + 1)
            ) : (
              <ContentCard
                card={card}
                href={p.href}
                shape={p.shape}
                groupKinds={p.groupKinds}
                chips={p.chips}
                tiers={p.tiers ?? kinds?.tiers}
                labels={p.labels}
                onOpen={() => exposure.click(card.id, i + 1)}
              />
            )}
          </li>
        ))}
      </ol>
    </ContentKitUiRoot>
  );
}

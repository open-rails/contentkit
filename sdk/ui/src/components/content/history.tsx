import { useMemo, useRef, useState } from "react";
import type { Card } from "../../client/generated/wire.js";
import type { HistoryQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { useHistory } from "../../react/discovery.js";
import { useKinds } from "../../react/entities.js";
import { Button } from "#ckui/ui/button";
import { formatDate, relativeTime, useAction } from "../social/parts.js";
import { ContentFilterDialog } from "./filter-dialog.js";
import { ContentCard } from "./card.js";
import { CardGrid, type CardListProps } from "./grid.js";

/** Account history or this browser's guest history, each removable, with a clear-all. */
export function ContentHistory({ query = {}, ...p }: CardListProps & { query?: HistoryQuery }) {
  const { t } = useMessages();
  const q = useMemo(() => ({ ...query, summary: true, mode: p.locked === "collapse" ? "lock" as const : p.locked === "hide" ? "hide" as const : query.mode, limit: query.limit ?? p.pageSize }), [query, p.pageSize, p.locked]);
  const list = useHistory(q, { client: p.client });
  const forget = useAction("history.forget");
  const { kinds } = useKinds({ client: p.client });
  const [filtersOpen, setFiltersOpen] = useState(false);
  const filterTrigger = useRef<HTMLButtonElement | null>(null);
  return (
    <div className="grid gap-3" data-ckui="content-history">
      {list.items.length > 0 && (
        <Button size="sm" variant="outline" className="justify-self-end" disabled={forget.pending} onClick={() => void forget.run(() => list.forget(query.kind ? { kind: query.kind } : {}))}>
          {t("contents.clearHistory")}
        </Button>
      )}
      {list.filterSummaryUnknown && p.showFilteredSummary !== false && (
        <Button ref={filterTrigger} variant="outline" className="justify-self-start" onClick={() => setFiltersOpen(true)}>{t("releases.manageFilters")}</Button>
      )}
      <ContentFilterDialog client={p.client} language={q.language} open={filtersOpen} onOpenChange={setFiltersOpen} returnFocus={() => filterTrigger.current} />
      <CardGrid
        surface="history"
        empty={t("contents.historyEmpty")}
        {...p}
        list={list}
        render={JSON.stringify(q)}
        filterLanguage={q.language}
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
              entityKinds={p.entityKinds}
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
              <HistoryProgress card={card} presentation={kinds?.kinds[card.kind]?.presentation} />
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

/** When the reader last read or watched it, and how far: pages for a reader, minutes for a player. */
function HistoryProgress({ card, presentation }: { card: Card; presentation?: string }) {
  const m = useMessages();
  const v = card.viewer;
  if (!v) return null;
  const done = v.completed ? v.progress_max : Math.min(v.progress, v.progress_max);
  const amount =
    v.progress_max > 0 && presentation === "reader"
      ? m.plural("contents.pageProgress", v.progress_max, { progress: done })
      : v.progress_max > 0 && presentation === "player"
        ? m.plural("contents.minuteProgress", Math.max(1, Math.round(v.progress_max / 60)), { progress: Math.round(done / 60) })
        : null;
  if (!v.last_read_at && !amount) return null;
  return (
    <p className="flex flex-wrap gap-x-2 text-xs tabular-nums text-muted-foreground" data-ckui="history-progress">
      {v.last_read_at && (
        <time dateTime={v.last_read_at} title={formatDate(m, v.last_read_at)}>
          {m.t(presentation === "player" ? "contents.watchedAgo" : "contents.readAgo", { time: relativeTime(m, v.last_read_at) })}
        </time>
      )}
      {amount && <span>{amount}</span>}
    </p>
  );
}

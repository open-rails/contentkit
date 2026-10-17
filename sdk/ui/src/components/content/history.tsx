import { useMemo, useRef, useState } from "react";
import type { HistoryQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { useHistory } from "../../react/discovery.js";
import { Button } from "#ckui/ui/button";
import { useAction } from "../social/parts.js";
import { ContentFilterDialog } from "./filter-dialog.js";
import { ContentCard } from "./card.js";
import { CardGrid, type CardListProps } from "./grid.js";

/** Account history or this browser's guest history, each removable, with a clear-all. */
export function ContentHistory({ query = {}, ...p }: CardListProps & { query?: HistoryQuery }) {
  const { t } = useMessages();
  const q = useMemo(() => ({ ...query, summary: true, mode: p.locked === "collapse" ? "lock" as const : p.locked === "hide" ? "hide" as const : query.mode, limit: query.limit ?? p.pageSize }), [query, p.pageSize, p.locked]);
  const list = useHistory(q, { client: p.client });
  const forget = useAction("history.forget");
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

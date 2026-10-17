import { cn } from "cn";
import { useMemo, useRef, useState } from "react";
import type { ContentQuery, SearchQuery } from "../../client/discovery.js";
import { useMessages } from "../../i18n/context.js";
import { useContentsPage, useSearchPage, type UseContentsPage } from "../../react/discovery.js";
import { ContentKitUiRoot } from "../../scope.js";
import { CardGrid, cardListMode, type CardListProps } from "./grid.js";
import { Pagination } from "./pagination.js";

const NUMBERED_PAGE_SIZE = 24;

/** The page a list shows: the host's, else its own, which starts again when the query changes. */
function usePage(p: CardListProps, query: unknown): [number, (page: number) => void] {
  const key = JSON.stringify(query);
  const [own, setOwn] = useState({ key, page: 1 });
  const ownPage = own.key === key ? own.page : 1;
  const page = Math.max(1, Math.floor(p.page ?? ownPage));
  return [
    page,
    (next) => {
      if (p.page === undefined) setOwn({ key, page: next });
      p.onPageChange?.(next);
    },
  ];
}

function NumberedCards({
  result,
  page,
  setPage,
  render,
  filterKinds,
  filterLanguage,
  ...p
}: CardListProps & { result: UseContentsPage; page: number; setPage: (page: number) => void; render: string; filterKinds?: readonly string[]; filterLanguage?: string }) {
  const top = useRef<HTMLDivElement>(null);
  const size = p.pageSize ?? NUMBERED_PAGE_SIZE;
  const total = result.summary?.total ?? 0;
  const list = useMemo(
    () => ({ ...result, summary: result.summary, loadingMore: false, hasMore: false, loadMore: async () => {} }),
    [result],
  );
  const go = (next: number, scroll: boolean) => {
    setPage(next);
    if (scroll) top.current?.scrollIntoView({ block: "start", behavior: "smooth" });
  };
  const controls = (bottom: boolean) =>
    !result.loading || total > 0 ? (
      <Pagination page={page} total={total} atLeast={result.summary?.truncated} pageSize={size} range={!bottom} pageHref={p.pageHref} onPageChange={(n) => go(n, bottom)} />
    ) : null;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="numbered-grid">
      <div ref={top} className="scroll-mt-20">
        {controls(false)}
      </div>
      <CardGrid {...p} className={undefined} list={list} render={render} filterKinds={filterKinds} filterLanguage={filterLanguage} />
      {list.items.length > 0 && controls(true)}
    </ContentKitUiRoot>
  );
}

/** ContentGrid's numbered pages: each page with the selection total. */
export function PagedContentGrid({ query, locked = "show", ...p }: CardListProps & { query: ContentQuery }) {
  const { page: _page, cursor: _cursor, ...base } = query;
  const q = useMemo<ContentQuery>(
    () => ({ ...base, summary: true, limit: base.limit ?? p.pageSize ?? NUMBERED_PAGE_SIZE, mode: cardListMode(locked, base.mode) }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(base), p.pageSize, locked],
  );
  const [page, setPage] = usePage(p, q);
  const result = useContentsPage(q, page, { client: p.client });
  return (
    <NumberedCards
      {...p}
      pageSize={q.limit}
      locked={locked}
      result={result}
      page={page}
      setPage={setPage}
      render={JSON.stringify([q, page])}
      filterKinds={query.kind ? [query.kind] : undefined}
      filterLanguage={query.display_language ?? query.language}
    />
  );
}

/** SearchResults' numbered pages; nothing until the query has text. */
export function PagedSearchResults({ query, ...p }: CardListProps & { query: SearchQuery }) {
  const { t } = useMessages();
  const { page: _page, cursor: _cursor, ...base } = query;
  const q = useMemo<SearchQuery>(
    () => ({ ...base, summary: true, limit: base.limit ?? p.pageSize ?? NUMBERED_PAGE_SIZE, mode: cardListMode(p.locked, base.mode) }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(base), p.pageSize, p.locked],
  );
  const [page, setPage] = usePage(p, q);
  const result = useSearchPage(q, page, { client: p.client });
  if (!query.q.trim()) return null;
  return (
    <NumberedCards
      surface="search"
      empty={t("contents.noResults", { q: query.q.trim() })}
      {...p}
      pageSize={q.limit}
      result={result}
      page={page}
      setPage={setPage}
      render={JSON.stringify([q, page])}
      filterKinds={query.kind ? [query.kind] : undefined}
      filterLanguage={query.display_language ?? query.language}
    />
  );
}

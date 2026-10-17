import { cn } from "cn";
import { useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/group-fields.js";
import type { GroupCard } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useGroups, useGroupsPage, type GroupFilter, type GroupSort } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
import { SortMenu } from "../content/listing.js";
import { Pagination } from "../content/pagination.js";
import { GroupCover, GroupLink, type GroupLike } from "./parts.js";

export interface GroupDirectoryProps {
  /** The group kind listed: tag, artist, series. */
  kind: string;
  /** Names, counts and paths in this language; default the provider's. */
  language?: string;
  /** Count contents of this kind only; default every kind. */
  contentKind?: string;
  /** Children of this group. */
  parent?: string;
  /** Groups scoped by this group (characters of a series). */
  scope?: string;
  /** Default "name". With sorts, the menu's choice; with onSortChange the host keeps it. */
  sort?: GroupSort;
  /** The sorts offered as a menu; none by default. */
  sorts?: readonly GroupSort[];
  onSortChange?: (sort: GroupSort) => void;
  /** The letter bucket shown, a-z or "#", null for every group. With onLetterChange the host keeps it (in its URL). */
  letter?: string | null;
  onLetterChange?: (letter: string | null) => void;
  /** Hide the A-Z bar. */
  letters?: boolean;
  /** "list" (names with counts, default) or "grid" (covers). */
  layout?: "list" | "grid";
  /** A group's link; default its canonical path. */
  href?: (group: GroupLike) => string | undefined;
  /** A group's entry instead of the default one. */
  renderGroup?: (group: GroupCard) => ReactNode;
  pageSize?: number;
  /** Numbered pages with a shown range instead of load more. */
  pagination?: "pages";
  /** The numbered page, from 1; with onPageChange the host keeps it. A changed letter or sort starts again at page 1: a host keeping the page drops it with them. */
  page?: number;
  onPageChange?: (page: number) => void;
  /** Each numbered page's link, for crawlers and opening in a new tab. */
  pageHref?: (page: number) => string;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A directory of one kind's groups: A-Z buckets, an optional sort, names with content counts or covers, paged. */
export function GroupDirectory(p: GroupDirectoryProps) {
  const m = useMessages();
  const { t } = m;
  const [own, setOwn] = useState<string | null>(null);
  const [ownSort, setOwnSort] = useState<GroupSort>("name");
  const letter = p.letter !== undefined ? p.letter : own;
  const sort = p.sort ?? ownSort;
  const choose = (l: string | null) => (p.onLetterChange ? p.onLetterChange(l) : setOwn(l));
  const chooseSort = (s: GroupSort) => (p.onSortChange ? p.onSortChange(s) : setOwnSort(s));
  const filter: GroupFilter = { kind: p.kind, language: p.language, contentKind: p.contentKind, parent: p.parent, scope: p.scope, sort, letter: letter ?? undefined, pageSize: p.pageSize };
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="group-directory" data-kind={p.kind}>
      {(p.letters !== false || !!p.sorts?.length) && (
        <div className="flex flex-wrap items-start justify-between gap-2">
          {p.letters !== false && (
            <nav aria-label={t("groups.byLetter")} className="flex flex-wrap gap-1" data-ckui="letters">
              {[null, ...LETTERS].map((l) => (
                <Button
                  key={l ?? "all"}
                  size="xs"
                  variant={letter === l ? "default" : "ghost"}
                  aria-pressed={letter === l}
                  aria-label={l === null ? undefined : l === "#" ? t("groups.otherLetters") : l.toUpperCase()}
                  className="min-w-7"
                  onClick={() => choose(l)}
                >
                  {l === null ? t("groups.all") : l.toUpperCase()}
                </Button>
              ))}
            </nav>
          )}
          {!!p.sorts?.length && <SortMenu value={sort} sorts={p.sorts} onChange={chooseSort} label={(s) => t(`groups.directorySort_${s}`)} className="ms-auto" />}
        </div>
      )}
      {p.pagination === "pages" ? <NumberedGroups {...p} filter={filter} /> : <CursorGroups {...p} filter={filter} />}
    </ContentKitUiRoot>
  );
}

type Shown = Pick<GroupDirectoryProps, "layout" | "href" | "renderGroup">;

function CursorGroups({ filter, client, ...p }: GroupDirectoryProps & { filter: GroupFilter }) {
  const m = useMessages();
  const { t } = m;
  const list = useGroups(filter, { client });
  return (
    <>
      <Groups {...p} items={list.items} loading={list.loading} error={list.error} reload={list.reload} />
      {list.hasMore && (
        <div className="grid justify-items-center gap-2">
          {list.error && <p role="alert" className="text-destructive">{contentError(m, list.error)}</p>}
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("groups.loading") : list.error ? t("common.retry") : t("groups.loadMore")}
          </Button>
        </div>
      )}
    </>
  );
}

function NumberedGroups({ filter, client, ...p }: GroupDirectoryProps & { filter: GroupFilter }) {
  const key = JSON.stringify(filter);
  const [own, setOwn] = useState({ key, page: 1 });
  const page = Math.max(1, Math.floor(p.page ?? (own.key === key ? own.page : 1)));
  const top = useRef<HTMLDivElement>(null);
  const r = useGroupsPage(filter, page, { client });
  const size = filter.pageSize ?? 50;
  const go = (next: number, scroll: boolean) => {
    if (p.page === undefined) setOwn({ key, page: next });
    p.onPageChange?.(next);
    if (scroll) top.current?.scrollIntoView({ block: "start", behavior: "smooth" });
  };
  const controls = (bottom: boolean) =>
    r.total !== undefined ? <Pagination page={page} total={r.total} pageSize={size} range={!bottom} pageHref={p.pageHref} onPageChange={(n) => go(n, bottom)} /> : null;
  return (
    <>
      <div ref={top} className="scroll-mt-20">
        {controls(false)}
      </div>
      <Groups {...p} items={r.items} loading={r.loading} error={r.error} reload={r.reload} />
      {r.items.length > 0 && controls(true)}
    </>
  );
}

function Groups({ items, loading, error, reload, ...p }: Shown & { items: GroupCard[]; loading: boolean; error?: Parameters<typeof contentError>[1]; reload: () => void }) {
  const m = useMessages();
  const { t } = m;
  const grid = p.layout === "grid";
  const count = (n: number) => new Intl.NumberFormat(m.language).format(n);
  if (loading && !items.length)
    return (
      <div className={grid ? "grid grid-cols-[repeat(auto-fill,minmax(9rem,1fr))] gap-3" : "grid gap-2"} aria-busy>
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className={grid ? "aspect-[3/4] w-full" : "h-6 w-48"} />
        ))}
      </div>
    );
  if (error && !items.length)
    return (
      <div role="alert" className="grid justify-items-start gap-2">
        <p className="text-destructive">{contentError(m, error)}</p>
        <Button size="sm" variant="outline" onClick={reload}>
          {t("common.retry")}
        </Button>
      </div>
    );
  if (!items.length)
    return (
      <p className="text-muted-foreground" role="status">
        {t("groups.empty")}
      </p>
    );
  return (
    <ul className={grid ? "grid grid-cols-[repeat(auto-fill,minmax(9rem,1fr))] gap-3" : "flex flex-wrap gap-2"} data-ckui="groups">
      {items.map((g) => (
        <li key={g.id} data-ckui="group" data-slug={g.slug}>
          {p.renderGroup ? (
            p.renderGroup(g)
          ) : grid ? (
            <GroupLink group={g} href={p.href} className="group grid gap-1.5">
              <GroupCover group={g} className="aspect-[3/4] w-full" />
              <span className="font-medium break-words group-hover:underline">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground tabular-nums">{m.plural("groups.items", g.count)}</span>
            </GroupLink>
          ) : (
            <GroupLink group={g} href={p.href} className="inline-flex items-center gap-1.5 rounded-md border border-border px-2 py-1 hover:bg-muted">
              <span className="break-words">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground tabular-nums" aria-label={m.plural("groups.items", g.count)}>
                {count(g.count)}
              </span>
            </GroupLink>
          )}
        </li>
      ))}
    </ul>
  );
}

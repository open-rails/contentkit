import { cn } from "cn";
import { useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/entity-fields.js";
import type { EntityCard } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useEntities, useEntitiesPage, type EntityFilter, type EntitySort } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
import { SortMenu } from "../content/listing.js";
import { Pagination } from "../content/pagination.js";
import { EntityCover, EntityLink, type EntityLike } from "./parts.js";

export interface EntityDirectoryProps {
  /** The entity kind listed: tag, artist, series. */
  kind: string;
  /** Names, counts and paths in this language; default the provider's. */
  language?: string;
  /** Count contents of this kind only; default every kind. */
  contentKind?: string;
  /** Children of this entity. */
  parent?: string;
  /** Entities scoped by this entity (characters of a series). */
  scope?: string;
  /** Default "name". With sorts, the menu's choice; with onSortChange the host keeps it. */
  sort?: EntitySort;
  /** The sorts offered as a menu; none by default. */
  sorts?: readonly EntitySort[];
  onSortChange?: (sort: EntitySort) => void;
  /** The letter bucket shown, a-z or "#", null for every entity. With onLetterChange the host keeps it (in its URL). */
  letter?: string | null;
  onLetterChange?: (letter: string | null) => void;
  /** Hide the A-Z bar. */
  letters?: boolean;
  /** "list" (names with counts, default) or "grid" (covers). */
  layout?: "list" | "grid";
  /** A entity's link; default its canonical path. */
  href?: (entity: EntityLike) => string | undefined;
  /** A entity's entry instead of the default one. */
  renderEntity?: (entity: EntityCard) => ReactNode;
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

/** A directory of one kind's entities: A-Z buckets, an optional sort, names with content counts or covers, paged. */
export function EntityDirectory(p: EntityDirectoryProps) {
  const m = useMessages();
  const { t } = m;
  const [own, setOwn] = useState<string | null>(null);
  const [ownSort, setOwnSort] = useState<EntitySort>("name");
  const letter = p.letter !== undefined ? p.letter : own;
  const sort = p.sort ?? ownSort;
  const choose = (l: string | null) => (p.onLetterChange ? p.onLetterChange(l) : setOwn(l));
  const chooseSort = (s: EntitySort) => (p.onSortChange ? p.onSortChange(s) : setOwnSort(s));
  const filter: EntityFilter = { kind: p.kind, language: p.language, contentKind: p.contentKind, parent: p.parent, scope: p.scope, sort, letter: letter ?? undefined, pageSize: p.pageSize };
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="entity-directory" data-kind={p.kind}>
      {(p.letters !== false || !!p.sorts?.length) && (
        <div className="flex flex-wrap items-start justify-between gap-2">
          {p.letters !== false && (
            <nav aria-label={t("entities.byLetter")} className="flex flex-wrap gap-1" data-ckui="letters">
              {[null, ...LETTERS].map((l) => (
                <Button
                  key={l ?? "all"}
                  size="xs"
                  variant={letter === l ? "default" : "ghost"}
                  aria-pressed={letter === l}
                  aria-label={l === null ? undefined : l === "#" ? t("entities.otherLetters") : l.toUpperCase()}
                  className="min-w-7"
                  onClick={() => choose(l)}
                >
                  {l === null ? t("entities.all") : l.toUpperCase()}
                </Button>
              ))}
            </nav>
          )}
          {!!p.sorts?.length && <SortMenu value={sort} sorts={p.sorts} onChange={chooseSort} label={(s) => t(`groups.directorySort_${s}`)} className="ms-auto" />}
        </div>
      )}
      {p.pagination === "pages" ? <NumberedEntities {...p} filter={filter} /> : <CursorEntities {...p} filter={filter} />}
    </ContentKitUiRoot>
  );
}

type Shown = Pick<EntityDirectoryProps, "layout" | "href" | "renderEntity">;

function CursorEntities({ filter, client, ...p }: EntityDirectoryProps & { filter: EntityFilter }) {
  const m = useMessages();
  const { t } = m;
  const list = useEntities(filter, { client });
  return (
    <>
      <Entities {...p} items={list.items} loading={list.loading} error={list.error} reload={list.reload} />
      {list.hasMore && (
        <div className="grid justify-items-center gap-2">
          {list.error && <p role="alert" className="text-destructive">{contentError(m, list.error)}</p>}
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("entities.loading") : list.error ? t("common.retry") : t("entities.loadMore")}
          </Button>
        </div>
      )}
    </>
  );
}

function NumberedEntities({ filter, client, ...p }: EntityDirectoryProps & { filter: EntityFilter }) {
  const key = JSON.stringify(filter);
  const [own, setOwn] = useState({ key, page: 1 });
  const page = Math.max(1, Math.floor(p.page ?? (own.key === key ? own.page : 1)));
  const top = useRef<HTMLDivElement>(null);
  const r = useEntitiesPage(filter, page, { client });
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
      <Entities {...p} items={r.items} loading={r.loading} error={r.error} reload={r.reload} />
      {r.items.length > 0 && controls(true)}
    </>
  );
}

function Entities({ items, loading, error, reload, ...p }: Shown & { items: EntityCard[]; loading: boolean; error?: Parameters<typeof contentError>[1]; reload: () => void }) {
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
        {t("entities.empty")}
      </p>
    );
  return (
    <ul className={grid ? "grid grid-cols-[repeat(auto-fill,minmax(9rem,1fr))] gap-3" : "flex flex-wrap gap-2"} data-ckui="entitys">
      {items.map((g) => (
        <li key={g.id} data-ckui="entity" data-slug={g.slug}>
          {p.renderEntity ? (
            p.renderEntity(g)
          ) : grid ? (
            <EntityLink entityy={g} href={p.href} className="group grid gap-1.5">
              <EntityCover entityy={g} className="aspect-[3/4] w-full" />
              <span className="font-medium break-words group-hover:underline">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground tabular-nums">{m.plural("entities.items", g.count)}</span>
            </EntityLink>
          ) : (
            <EntityLink entityy={g} href={p.href} className="inline-flex items-center gap-1.5 rounded-md border border-border px-2 py-1 hover:bg-muted">
              <span className="break-words">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground tabular-nums" aria-label={m.plural("entities.items", g.count)}>
                {count(g.count)}
              </span>
            </EntityLink>
          )}
        </li>
      ))}
    </ul>
  );
}

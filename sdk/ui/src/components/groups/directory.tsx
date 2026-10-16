import { cn } from "cn";
import { useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/group-fields.js";
import type { GroupCard } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useGroups, type GroupSort } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError } from "../social/parts.js";
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
  /** Default "name". */
  sort?: GroupSort;
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
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A directory of one kind's groups: A-Z buckets, names with content counts or covers, paged. */
export function GroupDirectory(p: GroupDirectoryProps) {
  const m = useMessages();
  const { t } = m;
  const [own, setOwn] = useState<string | null>(null);
  const letter = p.letter !== undefined ? p.letter : own;
  const choose = (l: string | null) => (p.onLetterChange ? p.onLetterChange(l) : setOwn(l));
  const list = useGroups(
    { kind: p.kind, language: p.language, contentKind: p.contentKind, parent: p.parent, scope: p.scope, sort: p.sort, letter: letter ?? undefined, pageSize: p.pageSize },
    { client: p.client },
  );
  const grid = p.layout === "grid";
  const count = (n: number) => new Intl.NumberFormat(m.language).format(n);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="group-directory" data-kind={p.kind}>
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
      {list.loading && !list.items.length ? (
        <div className={grid ? "grid grid-cols-[repeat(auto-fill,minmax(9rem,1fr))] gap-3" : "grid gap-2"} aria-busy>
          {Array.from({ length: 6 }, (_, i) => (
            <Skeleton key={i} className={grid ? "aspect-[3/4] w-full" : "h-6 w-48"} />
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
        <p className="text-muted-foreground" role="status">
          {t("groups.empty")}
        </p>
      ) : (
        <ul className={grid ? "grid grid-cols-[repeat(auto-fill,minmax(9rem,1fr))] gap-3" : "flex flex-wrap gap-2"} data-ckui="groups">
          {list.items.map((g) => (
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
      )}
      {list.hasMore && (
        <div className="grid justify-items-center gap-2">
          {list.error && <p role="alert" className="text-destructive">{contentError(m, list.error)}</p>}
          <Button variant="outline" size="sm" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("groups.loading") : list.error ? t("common.retry") : t("groups.loadMore")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

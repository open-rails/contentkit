import { cn } from "cn";
import { useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { GroupDetail } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useGroups, useKinds } from "../../react/groups.js";
import { Button } from "#ckui/ui/button";
import { GroupAvatar, GroupLink, labelOf, type GroupLike, type Labels } from "./parts.js";

export interface ScopedGroupsProps {
  /** The scope: e.g. a series, whose characters are listed. */
  group: Pick<GroupDetail, "id" | "kind">;
  /** Names and counts in this language; default the provider's. */
  language?: string;
  /** Section titles by kind route or id; default the route humanized ("characters" → "Characters"). */
  labels?: Labels;
  /** A group's link; default its canonical path. */
  href?: (group: GroupLike) => string | undefined;
  /** Opens a group in-app instead of linking (an admin editor). */
  onSelect?: (group: GroupLike) => void;
  /** Staff management: internal entities too. */
  management?: boolean;
  /** Shown before "Show more"; default 12. */
  limit?: number;
  client?: ContentKitClient;
  className?: string;
}

/** The entities scoped by a group, one section per kind scoped by its kind (a series' characters): avatar and name, most used first. */
export function ScopedGroups(p: ScopedGroupsProps) {
  const { kinds } = useKinds({ client: p.client });
  const scoped = Object.entries(kinds?.entity_kinds ?? {}).filter(([, k]) => k.scoped_by === p.group.kind);
  if (scoped.length === 0) return null;
  return (
    <div className={cn("grid gap-4", p.className)} data-ckui="scoped-groups">
      {scoped.map(([kind, spec]) => (
        <ScopedSection key={kind} {...p} kind={kind} title={labelOf(p.labels, spec.route ?? kind)} />
      ))}
    </div>
  );
}

function ScopedSection(p: ScopedGroupsProps & { kind: string; title: string }) {
  const { t } = useMessages();
  const id = useId();
  const limit = p.limit ?? 12;
  const [all, setAll] = useState(false);
  const list = useGroups({ kind: p.kind, scope: p.group.id, language: p.language, sort: "count" }, { client: p.client, management: p.management });
  if (list.items.length === 0) return null;
  const shown = all ? list.items : list.items.slice(0, limit);
  const more = (!all && list.items.length > limit) || (all && list.hasMore);
  return (
    <section aria-labelledby={id} className="grid gap-2" data-ckui="scoped-group-section" data-kind={p.kind}>
      <h2 id={id} className="text-sm font-semibold">
        {p.title}
      </h2>
      <ul className="flex flex-wrap gap-2">
        {shown.map((g) => {
          const body = (
            <>
              <GroupAvatar group={g} className="size-7 text-xs" />
              <span className="min-w-0 truncate">{g.name || g.slug}</span>
              {g.count > 0 && <span className="text-xs text-muted-foreground tabular-nums">{g.count}</span>}
            </>
          );
          const chip = "flex max-w-full items-center gap-2 rounded-full border border-border py-1 ps-1 pe-3 text-sm hover:bg-muted";
          return (
            <li key={g.id} className="max-w-full">
              {p.onSelect ? (
                <button type="button" className={chip} onClick={() => p.onSelect?.(g)}>
                  {body}
                </button>
              ) : (
                <GroupLink group={g} href={p.href} className={chip}>
                  {body}
                </GroupLink>
              )}
            </li>
          );
        })}
      </ul>
      {more && (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="justify-self-start"
          disabled={list.loadingMore}
          onClick={() => (all ? void list.loadMore() : setAll(true))}
        >
          {t("groups.showMore")}
        </Button>
      )}
    </section>
  );
}

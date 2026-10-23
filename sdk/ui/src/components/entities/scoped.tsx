import { cn } from "cn";
import { useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { EntityDetail } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useEntities, useKinds } from "../../react/entities.js";
import { Button } from "#ckui/ui/button";
import { EntityAvatar, EntityLink, labelOf, type EntityLike, type Labels } from "./parts.js";

export interface ScopedEntitiesProps {
  /** The scope: e.g. a series, whose characters are listed. */
  entity: Pick<EntityDetail, "id" | "kind">;
  /** Names and counts in this language; default the provider's. */
  language?: string;
  /** Section titles by kind route or id; default the route humanized ("characters" → "Characters"). */
  labels?: Labels;
  /** A entity's link; default its canonical path. */
  href?: (entity: EntityLike) => string | undefined;
  /** Opens a entity in-app instead of linking (an admin editor). */
  onSelect?: (entity: EntityLike) => void;
  /** Staff management: internal entities too. */
  management?: boolean;
  /** Shown before "Show more"; default 12. */
  limit?: number;
  client?: ContentKitClient;
  className?: string;
}

/** The entities scoped by a entity, one section per kind scoped by its kind (a series' characters): avatar and name, most used first. */
export function ScopedEntities(p: ScopedEntitiesProps) {
  const { kinds } = useKinds({ client: p.client });
  const scoped = Object.entries(kinds?.entity_kinds ?? {}).filter(([, k]) => k.scoped_by === p.entity.kind);
  if (scoped.length === 0) return null;
  return (
    <div className={cn("grid gap-4", p.className)} data-ckui="scoped-entitys">
      {scoped.map(([kind, spec]) => (
        <ScopedSection key={kind} {...p} kind={kind} title={labelOf(p.labels, spec.route ?? kind)} />
      ))}
    </div>
  );
}

function ScopedSection(p: ScopedEntitiesProps & { kind: string; title: string }) {
  const { t } = useMessages();
  const id = useId();
  const limit = p.limit ?? 12;
  const [all, setAll] = useState(false);
  const list = useEntities({ kind: p.kind, scope: p.entity.id, language: p.language, sort: "count" }, { client: p.client, management: p.management });
  if (list.items.length === 0) return null;
  const shown = all ? list.items : list.items.slice(0, limit);
  const more = (!all && list.items.length > limit) || (all && list.hasMore);
  return (
    <section aria-labelledby={id} className="grid gap-2" data-ckui="scoped-entity-section" data-kind={p.kind}>
      <h2 id={id} className="text-sm font-semibold">
        {p.title}
      </h2>
      <ul className="flex flex-wrap gap-2">
        {shown.map((g) => {
          const body = (
            <>
              <EntityAvatar entity={g} className="size-7 text-xs" />
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
                <EntityLink entity={g} href={p.href} className={chip}>
                  {body}
                </EntityLink>
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
          {t("entities.showMore")}
        </Button>
      )}
    </section>
  );
}

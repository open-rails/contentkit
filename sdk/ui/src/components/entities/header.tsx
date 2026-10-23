import { cn } from "cn";
import type { ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Attribute, EntityDetail } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { useEntity, useKinds } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError, formatDate } from "../social/parts.js";
import { EntityCover, EntityLink, labelOf, type EntityLike, type Labels } from "./parts.js";
import { ScopedEntities } from "./scoped.js";

export interface EntityHeaderProps {
  /** The entity the host already read; or give id. */
  entity?: EntityDetail;
  id?: string;
  /** Text, names and counts in this language; default the provider's. */
  language?: string;
  labels?: Labels;
  /** A entity's link (parent, scope, children); default its canonical path. */
  href?: (entity: EntityLike) => string | undefined;
  /** Below the description: the host's actions. */
  children?: ReactNode;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A value as its attribute's type shows it. */
export function formatAttribute(m: Translator, spec: Attribute | undefined, value: unknown, labels?: Labels): string {
  if (typeof value === "boolean") return m.t(value ? "entities.yes" : "entities.no");
  if (spec?.type === "time" && typeof value === "string") return formatDate(m, value);
  if (spec?.type === "int" && typeof value === "number") return new Intl.NumberFormat(m.language).format(value);
  if (spec?.type === "enum" && typeof value === "string") return labelOf(labels, value);
  return String(value);
}

/** A entity page's header: cover, name, other names, description, attributes, parent, scope, sub-entities and count. */
export function EntityHeader(p: EntityHeaderProps) {
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const read = useEntity(p.entity ? null : p.id, { language: p.language, client: p.client });
  const { kinds } = useKinds({ client: p.client });
  const g = p.entity ?? read.entity;
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="entity-header">
      {children}
    </ContentKitUiRoot>
  );
  if (!g) {
    if (read.error)
      return root(
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, read.error)}</p>
          <Button size="sm" variant="outline" onClick={read.reload}>
            {t("common.retry")}
          </Button>
        </div>,
      );
    return root(<Skeleton className="h-32 w-full" />);
  }
  const lang = p.language ?? g.name_language;
  const text = g.text.find((x) => x.language === lang) ?? g.text.find((x) => x.description);
  const others = g.names.filter((n) => n.name !== g.name);
  const declared = kinds?.entity_kinds?.[g.kind]?.attributes ?? {};
  const attributes = Object.entries(g.attributes).filter(([, v]) => v !== null && v !== undefined && v !== "");
  const count = g.counts.filter((c) => c.language === (p.language ?? "")).reduce((n, c) => n + c.count, 0);
  return root(
    <div className="grid gap-4 sm:grid-cols-[10rem_minmax(0,1fr)]" data-kind={g.kind}>
      <EntityCover entityy={g} className="aspect-[3/4] w-32 sm:w-40" />
      <div className="grid content-start gap-3">
        <div className="grid gap-1">
          {(g.parent || g.scope) && (
            <p className="text-xs text-muted-foreground" data-ckui="entity-parent">
              {[g.scope, g.parent]
                .filter((x, i, all): x is NonNullable<typeof x> => !!x && all.findIndex((y) => y?.id === x.id) === i)
                .map((x, i) => (
                  <span key={x.id}>
                    {i > 0 && " · "}
                    <EntityLink entityy={x} href={p.href} className="hover:underline">
                      {x.name || x.slug}
                    </EntityLink>
                  </span>
                ))}
            </p>
          )}
          <h1 className="text-xl font-semibold break-words">{g.name || g.slug}</h1>
          <p className="text-xs text-muted-foreground tabular-nums">{m.plural("entities.items", count)}</p>
        </div>
        {others.length > 0 && (
          <p className="text-muted-foreground" data-ckui="entity-names">
            <span className="sr-only">{t("entities.alsoKnownAs")} </span>
            {others.map((n, i) => (
              <span key={`${n.language}:${n.name}`} lang={n.language}>
                {i > 0 && " · "}
                {n.name}
                {n.kind === "name" && <span className="text-xs"> ({languageName(n.language)})</span>}
              </span>
            ))}
          </p>
        )}
        {text?.description && (
          <p className="break-words whitespace-pre-line" lang={text.language} data-ckui="entity-description">
            {text.description}
          </p>
        )}
        {attributes.length > 0 && (
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1" data-ckui="entity-attributes">
            {attributes.map(([name, v]) => (
              <div key={name} className="contents">
                <dt className="text-muted-foreground">{labelOf(p.labels, name)}</dt>
                <dd className="break-words">{formatAttribute(m, declared[name], v, p.labels)}</dd>
              </div>
            ))}
          </dl>
        )}
        {g.children.length > 0 && (
          <nav aria-label={t("entities.children")} className="flex flex-wrap gap-2" data-ckui="entity-children">
            {g.children.map((c) => (
              <EntityLink key={c.id} entityy={c} href={p.href} className="rounded-md border border-border px-2 py-1 hover:bg-muted">
                {c.name || c.slug}
              </EntityLink>
            ))}
          </nav>
        )}
        <ScopedEntities entityy={g} language={p.language} labels={p.labels} href={p.href} client={p.client} />
        {p.children}
      </div>
    </div>,
  );
}

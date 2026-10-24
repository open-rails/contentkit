import { cn } from "cn";
import { useCallback, useContext, useEffect, useId, useMemo, useState, type MouseEvent, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { humanize } from "../../client/entity-fields.js";
import type { Avatar, EntityCard, EntityDetail, EntitiesCover } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useContentKitClient } from "../../react/context.js";
import { useTypeahead } from "../../react/discovery.js";
import { Input } from "#ckui/ui/input";
import { useScopeProps } from "../../scope.js";
import { RenditionImg } from "../rendition-img.js";
import { ErrorLine } from "../social/parts.js";

/** A entity as covers and links need it. */
export type EntityLike = Pick<EntityCard, "id" | "kind" | "name" | "slug" | "path"> & { cover?: EntitiesCover; avatar?: Avatar };

/** Display names of kinds, attributes, roles and values by identifier; a missing one shows the identifier humanized. A kind may name one and many apart. */
export type Labels = Readonly<Record<string, string | { one: string; other: string }>>;

/** A label in its plural form unless one is asked for. */
export const labelOf = (labels: Labels | undefined, id: string, form: "one" | "other" = "other") => {
  const label = labels?.[id];
  return label === undefined ? humanize(id) : typeof label === "string" ? label : label[form];
};


export interface EntityCoverProps {
  entity: EntityLike;
  /** Shown instead, e.g. a cover just uploaded. */
  src?: string;
  className?: string;
}

/** A entity's cover or avatar image; its own styling root, so it renders anywhere in the host. */
function EntityImage({ entity, src, className, shape, slot }: EntityCoverProps & { shape: string; slot: "entity-cover" | "entity-avatar" }) {
  const scope = useScopeProps();
  const box = cn(scope.className, "overflow-hidden bg-muted", shape, className);
  const root = { "data-ckui-theme": scope["data-ckui-theme"], style: scope.style, "data-ckui": slot };
  const initial = (
    <span {...root} className={cn(box, "grid place-items-center font-semibold text-muted-foreground", slot === "entity-cover" && "text-lg")}>
      {(entity.name || entity.slug).trim().charAt(0).toUpperCase() || "?"}
    </span>
  );
  if (src) return <img {...root} src={src} alt="" className={cn(box, "object-cover")} />;
  const renditions = (slot === "entity-cover" ? entity.cover : entity.avatar)?.renditions;
  if (renditions?.length) return <RenditionImg {...root} outputs={renditions} fit="cover" skeleton className={box} fallback={initial} />;
  return initial;
}

/** A entity's cover: its renditions, else its initial on a muted tile. */
export function EntityCover(p: EntityCoverProps) {
  return <EntityImage {...p} shape="rounded-md" slot="entity-cover" />;
}

/** A entity's avatar: its renditions, else its initial in a circle. */
export function EntityAvatar(p: EntityCoverProps) {
  return <EntityImage {...p} shape="aspect-square rounded-full" slot="entity-avatar" />;
}

/** A link to a entity's page: `href`, else its canonical path; in-app through the provider's navigate. */
export function EntityLink({ entity, href, className, children }: { entity: EntityLike; href?: (g: EntityLike) => string | undefined; className?: string; children: ReactNode }) {
  const navigate = useContext(ContentKitContext)?.navigate;
  const url = href ? href(entity) : entity.path;
  const onClick = useCallback(
    (e: MouseEvent<HTMLAnchorElement>) => {
      if (!navigate || !url || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      navigate(url);
    },
    [navigate, url],
  );
  if (!url) return <span className={className}>{children}</span>;
  return (
    <a href={url} className={className} onClick={onClick}>
      {children}
    </a>
  );
}

export interface EntityPickerProps {
  kind: string;
  /** Names in this language. */
  language?: string;
  /** Entities not offered, by id. */
  exclude?: readonly string[];
  label: string;
  onPick: (entity: EntityCard) => void;
  client?: ContentKitClient;
  autoFocus?: boolean;
  /** Staff lookup, including internal entities. Requires entity-management permission. */
  management?: boolean;
  className?: string;
}

/** A typeahead over one kind's entities (GET /v1/typeahead). */
export function EntityPicker({ kind, language, exclude, label, onPick, client, autoFocus, className, management }: EntityPickerProps) {
  const { t } = useMessages();
  const id = useId();
  const [text, setText] = useState("");
  const [active, setActive] = useState(0);
  const found = useTypeahead(text, { entity_kinds: [kind], display_language: language, limit: 10, client, management });
  const results = useMemo(() => found.entities.filter((g) => g.kind === kind && !exclude?.includes(g.id)).slice(0, 8), [found.entities, kind, exclude]);
  const q = text.trim();
  useEffect(() => setActive(0), [q]);
  const pick = (g: EntityCard) => {
    setText("");
    onPick(g);
  };
  const open = results.length > 0;
  return (
    <div className={cn("relative grid min-w-48 flex-1 gap-1", className)} data-ckui="entity-picker">
      <Input
        role="combobox"
        aria-label={label}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        aria-autocomplete="list"
        autoComplete="off"
        className="h-8"
        value={text}
        placeholder={t("entities.searchPlaceholder")}
        autoFocus={autoFocus}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (!open) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setActive((a) => Math.min(results.length - 1, a + 1));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => Math.max(0, a - 1));
          } else if (e.key === "Enter") {
            e.preventDefault();
            pick(results[active]!);
          } else if (e.key === "Escape") setText("");
        }}
      />
      {open && (
        <ul id={`${id}-list`} role="listbox" aria-label={label} className="absolute top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
          {results.map((g, i) => (
            <li
              key={g.id}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cn("flex cursor-pointer items-baseline justify-between gap-2 px-3 py-1.5", i === active && "bg-accent text-accent-foreground")}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(g);
              }}
            >
              <span className="min-w-0 truncate">{g.name || g.slug}</span>
              <span className="text-xs text-muted-foreground">{g.slug}</span>
            </li>
          ))}
        </ul>
      )}
      {q && !open && found.query === q && !found.loading && !found.error && (
        <p className="text-xs text-muted-foreground" role="status">
          {t("entities.noMatch")}
        </p>
      )}
      <ErrorLine error={found.error} />
    </div>
  );
}

/** Entities read by id, for names where only ids are at hand (edges); a missing or deleted one is left out. */
export function useEntityNames(ids: readonly string[], o: { language?: string; client?: ContentKitClient; management?: boolean } = {}): ReadonlyMap<string, EntityDetail> {
  const client = useContentKitClient(o.client);
  const [found, setFound] = useState<ReadonlyMap<string, EntityDetail>>(new Map());
  const key = [...new Set(ids)].sort().join(",");
  useEffect(() => {
    if (!key) return;
    const ctl = new AbortController();
    const list = key.split(",");
    // A merged entity answers as its survivor: keep it under the id asked for.
    Promise.all(list.map((id) => (o.management ? client.entities.getForManagement(id, o.language, ctl.signal) : client.entities.get(id, o.language, ctl.signal)).catch(() => null))).then((all) => {
      if (!ctl.signal.aborted) setFound(new Map(all.flatMap((g, i) => (g ? [[list[i]!, g] as const] : []))));
    });
    return () => ctl.abort();
  }, [key, client, o.language, o.management]);
  return found;
}

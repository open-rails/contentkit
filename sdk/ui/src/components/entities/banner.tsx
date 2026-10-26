import { cn } from "cn";
import { type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { EntityDetail } from "../../client/generated/wire.js";
import type { EntityImageSlot } from "../../client/entities.js";
import { useMessages } from "../../i18n/context.js";
import { type ContentKitErrorHandler } from "../../react/context.js";
import { useEntity } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Skeleton } from "#ckui/ui/skeleton";
import { ProfileImageEditor } from "../profile-image-editor.js";
import { contentError, ErrorLine } from "../social/parts.js";
import { EntityAvatar, EntityCover, EntityLink, type EntityLike, type Labels } from "./parts.js";
import { ScopedEntities } from "./scoped.js";

export interface EntityBannerProps {
  /** The entity the host already read; or give id. */
  entity?: EntityDetail;
  id?: string;
  language?: string;
  /** Section titles of scoped entities (e.g. "characters"); default humanized. */
  labels?: Labels;
  /** A entity's link (scope, scoped entities); default its canonical path. */
  href?: (entity: EntityLike) => string | undefined;
  /** Beside the membership box: the host's actions (follow, share, edit). */
  children?: ReactNode;
  /** Show the cover across the top when the entity has one; false keeps the compact header. Default true. */
  cover?: boolean;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A entity's page head: its cover across, its avatar, scope, name, description, content count and membership, then the entities it scopes. */
export function EntityBanner(p: EntityBannerProps) {
  const m = useMessages();
  const read = useEntity(p.entity ? null : p.id, { language: p.language, client: p.client });
  const g = p.entity ?? read.entity;
  if (!g) return read.error ? <ErrorLine>{contentError(m, read.error)}</ErrorLine> : <Skeleton className="h-48 w-full" />;
  const text = g.text.find((x) => x.language === (p.language ?? g.name_language)) ?? g.text.find((x) => x.description);
  const count = g.counts.filter((c) => c.language === (p.language ?? "")).reduce((n, c) => n + c.count, 0);
  const cover = p.cover !== false && !!g.cover?.renditions?.length;
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="entity-banner" data-kind={g.kind}>
      {/* Without a cover image, a compact header rather than an empty 3:1 tile. Capped so the entity's contents start on the first screen. */}
      {cover && <EntityCover entity={g} className="aspect-[3/1] max-h-56 w-full" />}
      <div className={cn("flex flex-wrap gap-4 px-2 sm:px-4", cover ? "items-end" : "items-center")}>
        <EntityAvatar entity={g} className={cover ? "-mt-12 size-20 border-4 border-background sm:-mt-16 sm:size-28" : "size-16 sm:size-20"} />
        <div className="grid min-w-0 flex-1 gap-1">
          {g.scope && (
            <EntityLink entity={g.scope} href={p.href} className="text-xs text-muted-foreground hover:underline">
              {g.scope.name || g.scope.slug}
            </EntityLink>
          )}
          <h1 className="text-xl font-semibold break-words">{g.name || g.slug}</h1>
          <p className="text-xs text-muted-foreground tabular-nums">{m.plural("entities.items", count)}</p>
        </div>
        <div className="flex flex-wrap items-start gap-2">
          {p.children}
        </div>
      </div>
      {text?.description && <p className="px-2 break-words whitespace-pre-line sm:px-4">{text.description}</p>}
      <ScopedEntities entity={g} language={p.language} labels={p.labels} href={p.href} client={p.client} className="px-2 sm:px-4" />
    </ContentKitUiRoot>
  );
}

export interface EntityImageEditorProps {
  entity: EntityLike;
  /** Read the entity through the staff management API, including internal entities. */
  management?: boolean;
  /** The cover, or a user-owned kind's avatar. */
  slot: EntityImageSlot;
  /** Crop before uploading. Default true. */
  crop?: boolean;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
}

/** A metadata entity's cover or avatar editor. */
export function EntityImageEditor({ entity, client, management, ...props }: EntityImageEditorProps) {
  const edit = useEntity(entity.id, { client, management });
  return <ProfileImageEditor {...props} value={edit.entity ?? entity} preview={edit.previews[props.slot]} uploadImage={edit.uploadImage} update={edit.update} operation="entity.save" marker="entity-image-editor" />;
}

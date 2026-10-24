import { cn } from "cn";
import { useContext, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Edit, EntityAccess, EntityDetail } from "../../client/generated/wire.js";
import type { EntityImageSlot } from "../../client/entities.js";
import { decodeImage, type CropSource } from "../../client/image.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, type ContentKitErrorHandler } from "../../react/context.js";
import { useEntity } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { ImageCropDialog } from "../image-crop-dialog.js";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { EntityAvatar, EntityCover, EntityLink, type EntityLike, type Labels } from "./parts.js";
import { ScopedEntities } from "./scoped.js";

export interface MembershipBoxProps {
  entity: Pick<EntityDetail, "id" | "kind" | "name"> & { access?: EntityAccess };
  /** Opens the host billing flow for the entity entitlement. */
  onCheckout?: (o: { key: string }) => void;
  /** Asks a signed-out visitor to sign in; default the provider's. */
  onSignIn?: () => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Shows entity access; acquiring it is delegated to the host's billing flow. */
export function MembershipBox({ entity, onCheckout, onSignIn, className, appearance }: MembershipBoxProps) {
  const { t } = useMessages();
  const ctx = useContext(ContentKitContext);
  const a = entity.access;
  if (!a || a.editor || (!a.member && !a.unknown && !onCheckout)) return null;
  const signIn = onSignIn ?? ctx?.onSignIn;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-2 rounded-lg border border-border p-3 text-sm", className)} data-ckui="membership-box" data-member={a.member}>
      {a.unknown ? (
        <p className="text-muted-foreground">{t("membership.unknown")}</p>
      ) : a.member ? (
        <Badge>{t("membership.member")}</Badge>
      ) : (
        <div className="grid gap-2">
          <p className="text-muted-foreground">{t("membership.accessPitch")}</p>
          <Button size="sm" className="justify-self-start" onClick={() => {
            if (ctx?.viewer === null && signIn) return signIn();
            onCheckout?.({ key: a.key });
          }}>
            {t("membership.getAccess")}
          </Button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

export interface EntityBannerProps {
  /** The entity the host already read; or give id. */
  entity?: EntityDetail;
  id?: string;
  language?: string;
  onCheckout?: MembershipBoxProps["onCheckout"];
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
          <MembershipBox entity={g} onCheckout={p.onCheckout} client={p.client} />
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

/** A entity's cover or avatar for its editors: pick (and crop) an image, upload it and select it; or go back to none (a cover: automatic). */
export function EntityImageEditor({ entity, slot, crop = true, client, onError, className, management }: EntityImageEditorProps) {
  const { t } = useMessages();
  const g = useEntity(entity.id, { client, management });
  const input = useRef<HTMLInputElement>(null);
  const save = useAction("entity.save", onError);
  const [source, setSource] = useState<CropSource | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const shown = g.entity ?? entity;
  const set = slot === "cover" ? !!shown.cover?.image || (!!shown.cover?.content_id && !shown.cover.auto) : !!shown.avatar?.image;
  const upload = (f: Blob & { name?: string }, edit?: Edit) => save.run(() => g.uploadImage(slot, f, { wait: false, edit }));
  const close = () => {
    source?.revoke?.();
    setSource(null);
    setFile(null);
  };
  return (
    <div className={cn("flex flex-wrap items-center gap-3", className)} data-ckui="entity-image-editor" data-slot={slot}>
      {slot === "avatar" ? <EntityAvatar entity={shown} src={g.previews.avatar} className="size-28 shrink-0" /> : <EntityCover entity={shown} src={g.previews.cover} className="aspect-[3/1] w-72 max-w-full" />}
      <div className="grid gap-2">
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" variant="outline" disabled={save.pending} onClick={() => input.current?.click()}>
            {save.pending ? t("entityEditor.uploading") : t(slot === "avatar" ? "entityEditor.uploadAvatar" : "entityEditor.uploadCover")}
          </Button>
          {set && (
            <Button type="button" size="sm" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => g.update(slot === "cover" ? { cover: {} } : { avatar: {} }))}>
              {t(slot === "avatar" ? "entityEditor.removeAvatar" : "entityEditor.coverUseAuto")}
            </Button>
          )}
        </div>
        <input
          ref={input}
          type="file"
          accept="image/*"
          hidden
          aria-label={t(slot === "avatar" ? "entityEditor.uploadAvatar" : "entityEditor.uploadCover")}
          onChange={(e) => {
            const f = e.target.files?.[0];
            e.target.value = "";
            if (!f) return;
            if (!crop) return void upload(f);
            decodeImage(f).then(
              (s) => {
                setFile(f);
                setSource(s);
              },
              () => void upload(f),
            );
          }}
        />
        <ErrorLine error={save.error} />
      </div>
      <ImageCropDialog
        open={!!source}
        onOpenChange={(open) => !open && close()}
        source={source}
        aspect={slot === "avatar" ? "1:1" : "3:1"}
        aspects={slot === "avatar" ? undefined : ["3:1", "16:9", ""]}
        round={slot === "avatar"}
        onConfirm={(edit) => {
          const f = file;
          close();
          if (f) void upload(f, edit ?? undefined);
        }}
      />
    </div>
  );
}

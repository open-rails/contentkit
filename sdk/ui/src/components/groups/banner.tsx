import { cn } from "cn";
import { useContext, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Edit, GroupAccess, GroupDetail } from "../../client/generated/wire.js";
import type { GroupImageSlot } from "../../client/groups.js";
import { decodeImage, type CropSource } from "../../client/image.js";
import type { DecisionOffer } from "../../client/offers.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useGroup } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { ImageCropDialog } from "../image-crop-dialog.js";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { priceLabel } from "../content/paywall.js";
import { GroupAvatar, GroupCover, type GroupLike } from "./parts.js";

export interface MembershipBoxProps {
  group: Pick<GroupDetail, "id" | "kind" | "name"> & { access?: GroupAccess };
  /** Buys a sold membership: the host creates the order through billing-ui. */
  onCheckout?: (o: { key: string; offer: DecisionOffer }) => void;
  /** Asks a signed-out visitor to sign in; default the provider's. */
  onSignIn?: () => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A group's membership for the reader: member, join free, or join for its price. */
export function MembershipBox({ group, onCheckout, onSignIn, client: own, onError, className, appearance }: MembershipBoxProps) {
  const m = useMessages();
  const { t } = m;
  const client = useContentKitClient(own);
  const ctx = useContext(ContentKitContext);
  const join = useAction("membership.join", onError);
  const a = group.access;
  if (!a) return null;
  const signedOut = ctx?.viewer === null;
  const signIn = onSignIn ?? ctx?.onSignIn;
  const offer: DecisionOffer | undefined = a.offer ? { key: a.key, target: { kind: "group", id: group.id }, terms: a.offer } : undefined;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-2 rounded-lg border border-border p-3 text-sm", className)} data-ckui="membership-box" data-member={a.member}>
      {a.unknown ? (
        <p className="text-muted-foreground">{t("membership.unknown")}</p>
      ) : a.member ? (
        <div className="flex flex-wrap items-center gap-2">
          <Badge>{t("membership.member")}</Badge>
          {!offer && (
            <Button size="sm" variant="ghost" disabled={join.pending} onClick={() => void join.run(() => client.offers.join(group.id, false))}>
              {t("membership.leave")}
            </Button>
          )}
        </div>
      ) : (
        <div className="grid gap-2">
          <p className="text-muted-foreground">{t(offer ? "membership.paidPitch" : "membership.freePitch")}</p>
          <Button
            size="sm"
            className="justify-self-start"
            disabled={join.pending || (!!offer && !onCheckout)}
            onClick={() => {
              if (signedOut && signIn) return signIn();
              if (offer) return onCheckout?.({ key: a.key, offer });
              void join.run(() => client.offers.join(group.id));
            }}
          >
            {offer ? t("membership.joinFor", { price: priceLabel(m, offer.terms) }) : t("membership.join")}
          </Button>
        </div>
      )}
      {join.error && <ErrorLine>{contentError(m, join.error)}</ErrorLine>}
    </ContentKitUiRoot>
  );
}

export interface GroupBannerProps {
  /** The group the host already read; or give id. */
  group?: GroupDetail;
  id?: string;
  language?: string;
  onCheckout?: MembershipBoxProps["onCheckout"];
  /** Beside the membership box: the host's actions (follow, share, edit). */
  children?: ReactNode;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A group's page head: its cover across, its avatar, name, description, content count and membership. */
export function GroupBanner(p: GroupBannerProps) {
  const m = useMessages();
  const read = useGroup(p.group ? null : p.id, { language: p.language, client: p.client });
  const g = p.group ?? read.group;
  if (!g) return read.error ? <ErrorLine>{contentError(m, read.error)}</ErrorLine> : <Skeleton className="h-48 w-full" />;
  const text = g.text.find((x) => x.language === (p.language ?? g.name_language)) ?? g.text.find((x) => x.description);
  const count = g.counts.filter((c) => c.language === (p.language ?? "")).reduce((n, c) => n + c.count, 0);
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="group-banner" data-kind={g.kind}>
      <GroupCover group={g} className="aspect-[3/1] w-full" />
      <div className="flex flex-wrap items-end gap-4 px-2 sm:px-4">
        <GroupAvatar group={g} className="-mt-12 size-20 border-4 border-background sm:-mt-16 sm:size-28" />
        <div className="grid min-w-0 flex-1 gap-1">
          <h1 className="text-xl font-semibold break-words">{g.name || g.slug}</h1>
          <p className="text-xs text-muted-foreground tabular-nums">{m.plural("groups.items", count)}</p>
        </div>
        <div className="flex flex-wrap items-start gap-2">
          <MembershipBox group={g} onCheckout={p.onCheckout} client={p.client} />
          {p.children}
        </div>
      </div>
      {text?.description && <p className="px-2 break-words whitespace-pre-line sm:px-4">{text.description}</p>}
    </ContentKitUiRoot>
  );
}

export interface GroupImageEditorProps {
  group: GroupLike;
  /** The cover, or a user-owned kind's avatar. */
  slot: GroupImageSlot;
  /** Crop before uploading. Default true. */
  crop?: boolean;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
}

/** A group's cover or avatar for its editors: pick (and crop) an image, upload it and select it; or go back to none (a cover: automatic). */
export function GroupImageEditor({ group, slot, crop = true, client, onError, className }: GroupImageEditorProps) {
  const { t } = useMessages();
  const g = useGroup(group.id, { client });
  const input = useRef<HTMLInputElement>(null);
  const save = useAction("group.save", onError);
  const [source, setSource] = useState<CropSource | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const shown = g.group ?? group;
  const set = slot === "cover" ? !!shown.cover?.image || (!!shown.cover?.content_id && !shown.cover.auto) : !!shown.avatar?.image;
  const upload = (f: Blob & { name?: string }, edit?: Edit) => save.run(() => g.uploadImage(slot, f, { wait: false, edit }));
  const close = () => {
    source?.revoke?.();
    setSource(null);
    setFile(null);
  };
  return (
    <div className={cn("flex flex-wrap items-center gap-3", className)} data-ckui="group-image-editor" data-slot={slot}>
      {slot === "avatar" ? <GroupAvatar group={shown} src={g.previews.avatar} className="size-16" /> : <GroupCover group={shown} src={g.previews.cover} className="aspect-[3/1] w-48" />}
      <div className="grid gap-2">
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" variant="outline" disabled={save.pending} onClick={() => input.current?.click()}>
            {save.pending ? t("groupEditor.uploading") : t(slot === "avatar" ? "groupEditor.uploadAvatar" : "groupEditor.uploadCover")}
          </Button>
          {set && (
            <Button type="button" size="sm" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => g.update(slot === "cover" ? { cover: {} } : { avatar: {} }))}>
              {t(slot === "avatar" ? "groupEditor.removeAvatar" : "groupEditor.coverUseAuto")}
            </Button>
          )}
        </div>
        <input
          ref={input}
          type="file"
          accept="image/*"
          hidden
          aria-label={t(slot === "avatar" ? "groupEditor.uploadAvatar" : "groupEditor.uploadCover")}
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

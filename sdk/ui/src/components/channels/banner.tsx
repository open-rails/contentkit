import { cn } from "cn";
import type { ReactNode } from "react";
import type { Channel } from "../../client/generated/wire.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ChannelImageSlot } from "../../client/channels.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useChannel } from "../../react/channels.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError, ErrorLine } from "../social/parts.js";
import { ProfileImageEditor } from "../profile-image-editor.js";
import { EntityLink } from "../entities/parts.js";
import { ChannelAvatar, ChannelCover, type ChannelLike } from "./parts.js";
import { ChannelMembershipBox, type ChannelMembershipBoxProps } from "./membership.js";

export interface ChannelBannerProps {
  channel?: Channel;
  id?: string;
  language?: string;
  onCheckout?: ChannelMembershipBoxProps["onCheckout"];
  children?: ReactNode;
  cover?: boolean;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

export function ChannelBanner(props: ChannelBannerProps) {
  const messages = useMessages();
  const read = useChannel(props.channel ? null : props.id, { language: props.language, client: props.client });
  const channel = props.channel ?? read.channel;
  if (!channel) return read.error ? <ErrorLine>{contentError(messages, read.error)}</ErrorLine> : <Skeleton className="h-48 w-full" />;
  const text = channel.text.find((entry) => entry.language === (props.language ?? channel.name_language)) ?? channel.text.find((entry) => entry.description);
  const cover = props.cover !== false && !!channel.cover?.renditions?.length;
  return <ContentKitUiRoot appearance={props.appearance} className={cn("grid gap-3 text-sm", props.className)} data-ckui="channel-banner">
    {cover && <ChannelCover channel={channel} className="aspect-[3/1] max-h-56 w-full" />}
    <div className={cn("flex flex-wrap gap-4 px-2 sm:px-4", cover ? "items-end" : "items-center")}>
      <ChannelAvatar channel={channel} className={cover ? "-mt-12 size-20 border-4 border-background sm:-mt-16 sm:size-28" : "size-16 sm:size-20"} />
      <div className="grid min-w-0 flex-1 gap-1"><h1 className="text-xl font-semibold break-words">{channel.name || channel.slug}</h1><p className="text-muted-foreground">@{channel.slug}</p></div>
      <div className="flex flex-wrap items-start gap-2"><ChannelMembershipBox channel={channel} onCheckout={props.onCheckout} />{props.children}</div>
    </div>
    {text?.description && <p className="px-2 break-words whitespace-pre-line sm:px-4">{text.description}</p>}
    {!!channel.entities.length && <div className="flex flex-wrap gap-2 px-2 sm:px-4">{channel.entities.map((entity) => <EntityLink key={entity.id} entity={entity} className="rounded border px-2 py-1">{entity.name || entity.slug}</EntityLink>)}</div>}
  </ContentKitUiRoot>;
}

export interface ChannelImageEditorProps { channel: ChannelLike; slot: ChannelImageSlot; crop?: boolean; client?: ContentKitClient; onError?: ContentKitErrorHandler; className?: string }
export function ChannelImageEditor({ channel, client, ...props }: ChannelImageEditorProps) {
  const edit = useChannel(channel.id, { client });
  return <ProfileImageEditor {...props} value={edit.channel ?? channel} preview={edit.previews[props.slot]} uploadImage={edit.uploadImage} update={edit.update} operation="channel.save" marker="channel-image-editor" />;
}

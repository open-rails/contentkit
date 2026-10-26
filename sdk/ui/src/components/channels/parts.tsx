import { useCallback, useContext, type MouseEvent, type ReactNode } from "react";
import type { ChannelCard } from "../../client/generated/wire.js";
import { ContentKitContext } from "../../react/context.js";
import { ProfileImage } from "../profile-image.js";

export type ChannelLike = Pick<ChannelCard, "id" | "slug" | "name" | "path" | "cover" | "avatar">;
export interface ChannelImageProps { channel: ChannelLike; src?: string; className?: string }
export function ChannelAvatar({ channel, ...props }: ChannelImageProps) { return <ProfileImage {...props} value={channel} slot="avatar" marker="channel-avatar" />; }
export function ChannelCover({ channel, ...props }: ChannelImageProps) { return <ProfileImage {...props} value={channel} slot="cover" marker="channel-cover" />; }
export function ChannelLink({ channel, href, className, children }: { channel: ChannelLike; href?: (channel: ChannelLike) => string | undefined; className?: string; children: ReactNode }) {
  const navigate = useContext(ContentKitContext)?.navigate;
  const url = href ? href(channel) : channel.path;
  const click = useCallback((event: MouseEvent<HTMLAnchorElement>) => {
    if (!navigate || !url || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault(); navigate(url);
  }, [navigate, url]);
  return url ? <a href={url} onClick={click} className={className}>{children}</a> : <span className={className}>{children}</span>;
}

import type { ChannelContentsQuery } from "../../client/channels.js";
import { useChannelContents } from "../../react/channels.js";
import { CardGrid, type CardListProps } from "../content/grid.js";

export interface ChannelFeedProps extends CardListProps { channelId: string; query?: ChannelContentsQuery }
/** Originals and repost references, using the same cards and reader access as other lists. */
export function ChannelFeed({ channelId, query, ...props }: ChannelFeedProps) {
  const list = useChannelContents(channelId, query, { client: props.client });
  return <CardGrid {...props} list={list} render={`channel:${channelId}`} surface={props.surface ?? "channel"} />;
}

import type { ChannelContentsQuery } from "../../client/channels.js";
import { useChannel, useChannelContents } from "../../react/channels.js";
import { useMessages } from "../../i18n/context.js";
import { CardGrid, type CardListProps } from "../content/grid.js";
import { ContentCard } from "../content/card.js";
import { RepostButton } from "./repost.js";

export interface ChannelFeedProps extends CardListProps { channelId: string; query?: ChannelContentsQuery }
/** Originals and repost references, using the same cards and reader access as other lists. */
export function ChannelFeed({ channelId, query, renderCard, ...props }: ChannelFeedProps) {
  const list = useChannelContents(channelId, query, { client: props.client });
  const channel = useChannel(channelId, { client: props.client });
  const { t } = useMessages();
  return <CardGrid {...props} list={list} render={`channel:${channelId}`} surface={props.surface ?? "channel"} renderCard={(card, position) => <div className="grid gap-2">
    {card.repost && <div className="flex flex-wrap items-center justify-between gap-2 text-sm text-muted-foreground" data-ckui="repost-context">
      <span>{t("channels.repostedBy", { name: channel.channel?.name || channel.channel?.slug || t("channels.channel") })}</span>
      {card.repost.can_remove && <RepostButton contentId={card.id} removeFrom={card.repost.channel_id} client={props.client} />}
    </div>}
    {renderCard ? renderCard(card, position) : <ContentCard {...props} card={card} />}
  </div>} />;
}

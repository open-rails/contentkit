import { useContext, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { useMyChannels } from "../../react/channels.js";
import { ContentKitContext, useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useMessages } from "../../i18n/context.js";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from "#ckui/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { contentError, ErrorLine, useAction } from "../social/parts.js";

export interface RepostButtonProps {
  contentId: string;
  originalChannelId?: string;
  /** A feed reference the viewer can remove; never deletes the original. */
  removeFrom?: string;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  onChanged?: () => void;
  className?: string;
}

export function RepostButton({ contentId, originalChannelId, removeFrom, client: own, onError, onChanged, className }: RepostButtonProps) {
  const client = useContentKitClient(own);
  const context = useContext(ContentKitContext);
  const messages = useMessages();
  const [open, setOpen] = useState(false);
  const [chosen, setChosen] = useState("");
  const channels = useMyChannels({ client, enabled: open && !removeFrom });
  const destinations = channels.items.filter((channel) => channel.can_post && channel.id !== originalChannelId);
  const selected = destinations.find((channel) => channel.id === chosen)?.id ?? destinations[0]?.id;
  const action = useAction("channel.repost", onError);
  const save = async () => {
    if (!removeFrom && !selected) return;
    const done = await action.run(async () => {
      if (removeFrom) await client.channels.removeRepost(removeFrom, contentId);
      else await client.channels.repost(selected!, contentId);
      return true;
    });
    if (done !== undefined) { setOpen(false); onChanged?.(); }
  };
  return <>
    <Button type="button" size="sm" variant="outline" className={className} onClick={() => {
      if (context?.viewer === null && context.onSignIn) return context.onSignIn();
      setOpen(true);
    }}>{messages.t(removeFrom ? "channels.removeRepost" : "channels.repost")}</Button>
    <Dialog open={open} onOpenChange={setOpen}><DialogContent>
      <DialogHeader><DialogTitle>{messages.t(removeFrom ? "channels.removeRepost" : "channels.repost")}</DialogTitle><DialogDescription>{messages.t(removeFrom ? "channels.removeHint" : "channels.repostHint")}</DialogDescription></DialogHeader>
      {!removeFrom && <>
        {destinations.length > 0 ? <Select value={selected ?? ""} onValueChange={(value) => setChosen(String(value))}><SelectTrigger aria-label={messages.t("channels.destination")}><SelectValue>{(value: string) => destinations.find((channel) => channel.id === value)?.name ?? value}</SelectValue></SelectTrigger><SelectContent>{destinations.map((channel) => <SelectItem key={channel.id} value={channel.id}>{channel.name || channel.slug}</SelectItem>)}</SelectContent></Select> : <p>{messages.t(channels.loading ? "channels.loading" : "channels.noDestination")}</p>}
        {channels.error && <ErrorLine>{contentError(messages, channels.error)}</ErrorLine>}
        {(channels.hasMore || channels.error) && <Button type="button" variant="ghost" disabled={channels.loadingMore || channels.loading} onClick={() => channels.error ? channels.reload() : void channels.loadMore()}>{messages.t(channels.error ? "common.retry" : "entities.loadMore")}</Button>}
      </>}
      <ErrorLine error={action.error} />
      <DialogFooter><Button type="button" variant="ghost" onClick={() => setOpen(false)}>{messages.t("common.cancel")}</Button><Button type="button" disabled={action.pending || (!removeFrom && !selected)} onClick={() => void save()}>{messages.t(action.pending ? "common.saving" : removeFrom ? "channels.removeRepost" : "channels.repost")}</Button></DialogFooter>
    </DialogContent></Dialog>
  </>;
}

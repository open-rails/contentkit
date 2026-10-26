import { cn } from "cn";
import { useId, useState, type ReactNode } from "react";
import type { ChannelCard } from "../../client/generated/wire.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ChannelQuery } from "../../client/channels.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useChannels } from "../../react/channels.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Skeleton } from "#ckui/ui/skeleton";
import { contentError, ErrorLine } from "../social/parts.js";
import { ChannelCover, ChannelLink, type ChannelLike } from "./parts.js";

export interface ChannelDirectoryProps {
  query?: ChannelQuery;
  href?: (channel: ChannelLike) => string | undefined;
  renderChannel?: (channel: ChannelCard) => ReactNode;
  empty?: ReactNode;
  client?: ContentKitClient;
  className?: string;
  appearance?: ContentKitUiAppearance;
}
export function ChannelDirectory({ query, href, renderChannel, empty, client, className, appearance }: ChannelDirectoryProps) {
  const messages = useMessages();
  const list = useChannels(query, { client });
  return <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4", className)} data-ckui="channel-directory">
    {list.loading && !list.items.length ? <Skeleton className="h-40" /> : !list.items.length && !list.error ? empty : null}
    <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,15rem),1fr))] gap-4">{list.items.map((channel) => <div key={channel.id}>{renderChannel ? renderChannel(channel) : <ChannelLink channel={channel} href={href} className="grid gap-2 rounded-lg border border-border p-3"><ChannelCover channel={channel} className="aspect-[3/1] w-full" /><strong>{channel.name || channel.slug}</strong><span className="text-sm text-muted-foreground">@{channel.slug}</span></ChannelLink>}</div>)}</div>
    {list.error && <ErrorLine>{contentError(messages, list.error)}</ErrorLine>}
    {(list.hasMore || list.error) && <Button variant="outline" disabled={list.loadingMore || list.loading} onClick={() => list.error ? list.reload() : void list.loadMore()}>{messages.t(list.error ? "common.retry" : "entities.loadMore")}</Button>}
  </ContentKitUiRoot>;
}

export function ChannelPicker({ label, onPick, client }: { label: string; onPick: (channel: ChannelCard) => void; client?: ContentKitClient }) {
  const id = useId();
  const [query, setQuery] = useState("");
  const [focused, setFocused] = useState(false);
  const [active, setActive] = useState(0);
  const results = useChannels({ q: query.trim() || undefined, limit: 10 }, { client });
  const messages = useMessages();
  const pick = (channel: ChannelCard) => { onPick(channel); setQuery(""); setFocused(false); };
  return <div className="relative min-w-48 flex-1" data-ckui="channel-picker">
    <Input role="combobox" aria-label={label} aria-expanded={focused} aria-controls={`${id}-options`} aria-activedescendant={focused && results.items[active] ? `${id}-${active}` : undefined} autoComplete="off" value={query} onFocus={() => setFocused(true)} onBlur={() => setFocused(false)} onChange={(event) => { setQuery(event.target.value); setActive(0); }} onKeyDown={(event) => {
      if (event.key === "Escape") { setFocused(false); return; }
      if (event.key === "ArrowDown" || event.key === "ArrowUp") { event.preventDefault(); setFocused(true); setActive((n) => Math.max(0, Math.min(results.items.length - 1, n + (event.key === "ArrowDown" ? 1 : -1)))); }
      if (event.key === "Enter" && focused && results.items[active]) { event.preventDefault(); pick(results.items[active]); }
    }} />
    {focused && <div id={`${id}-options`} role="listbox" className="absolute z-20 mt-1 grid max-h-64 w-full overflow-auto rounded-md border bg-background p-1 shadow-lg">
      {results.items.map((channel, index) => <button key={channel.id} id={`${id}-${index}`} type="button" role="option" aria-selected={index === active} className={cn("rounded px-2 py-2 text-start", index === active && "bg-muted")} onMouseDown={(event) => event.preventDefault()} onClick={() => pick(channel)}>{channel.name || channel.slug}</button>)}
      {results.error && <ErrorLine>{contentError(messages, results.error)}</ErrorLine>}
    </div>}
  </div>;
}

import { useCallback, useEffect, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { Channel, ChannelInput, ChannelUpdate, ChannelCard, MyChannel, Card, Edit } from "../client/generated/wire.js";
import type { ChannelQuery, ChannelContentsQuery, ChannelImageSlot } from "../client/channels.js";
import { useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource } from "./use-resource.js";

export function useChannels(query: ChannelQuery = {}, options: { client?: ContentKitClient; enabled?: boolean } = {}) {
  const { client, store, scope, language } = useContentScope(options.client);
  const q = { language, ...query };
  return useList<ChannelCard>(store, options.enabled === false ? null : keyOf("channels", scope, q), { type: "channels" }, async (cursor, signal) => {
    const page = await client.channels.list({ ...q, cursor: cursor as string | undefined }, signal);
    return { items: page.channels, next: page.next_cursor ?? null };
  });
}

/** Current permissions from ContentKit, without assuming AuthKit group IDs equal channel IDs. */
export function useMyChannels(options: { client?: ContentKitClient; enabled?: boolean } = {}) {
  const { client, store, scope, language, viewer } = useContentScope(options.client);
  return useList<MyChannel>(store, viewer === null || options.enabled === false ? null : keyOf("channels", "mine", scope, language), { type: "channels-mine" }, async (cursor, signal) => {
    const page = await client.channels.mine({ language, cursor: cursor as string | undefined, limit: 100 }, signal);
    return { items: page.channels, next: page.next_cursor ?? null };
  });
}

export function useChannelContents(id: string | null | undefined, query: ChannelContentsQuery = {}, options: { client?: ContentKitClient } = {}) {
  const { client, store, scope, language } = useContentScope(options.client);
  const q = { language, ...query };
  return useList<Card>(store, id ? keyOf("channel-feed", scope, id, q) : null, { type: "channel-feed", id: id ?? undefined }, async (cursor, signal) => {
    const page = await client.channels.contents(id!, { ...q, cursor: cursor as string | undefined }, signal);
    return { items: page.contents, next: page.next_cursor ?? null };
  });
}

/** A channel's identity and current edits; all versions of its posts use this authoring home. */
export function useChannel(id: string | null | undefined, options: { client?: ContentKitClient; language?: string; initial?: Channel } = {}) {
  const { client, store, scope, language } = useContentScope(options.client);
  const guard = useGuarded();
  const [created, setCreated] = useState<{ id: string; scope: string; client: ContentKitClient }>();
  const channelId = id ?? (created?.scope === scope && created.client === client ? created.id : undefined);
  const previewKey = keyOf(scope, channelId);
  const [saving, setSaving] = useState(0);
  const [previews, setPreviews] = useState<{ client: ContentKitClient; key: string; images: Partial<Record<ChannelImageSlot, string>> }>({ client, key: previewKey, images: {} });
  const blobs = useRef<string[]>([]);
  useEffect(() => { const urls = blobs.current; return () => urls.forEach((url) => URL.revokeObjectURL(url)); }, []);
  const resource = useResource<Channel>(store, channelId ? keyOf("channel", scope, channelId, options.language ?? language) : null, { type: "channel", id: channelId }, (signal) => client.channels.get(channelId!, options.language ?? language, signal), { initial: options.initial });
  const run = useCallback(async <T,>(operation: () => Promise<T>) => {
    setSaving((n) => n + 1);
    try { return await guard(operation); } finally { setSaving((n) => n - 1); }
  }, [guard]);
  const need = () => { if (!channelId) throw new Error("Create the channel first."); return channelId; };
  return {
    channel: resource.data ?? null,
    loading: !!channelId && (resource.loading || !resource.loaded),
    error: resource.error,
    saving: saving > 0,
    reload: resource.reload,
    previews: previews.client === client && previews.key === previewKey ? previews.images : {},
    create: (input: ChannelInput) => run(async () => { const channel = await client.channels.create(input); setCreated({ id: channel.id, scope, client }); return channel; }),
    update: (patch: ChannelUpdate) => run(() => client.channels.update(need(), patch)),
    remove: () => run(() => client.channels.remove(need())),
    uploadImage: (slot: ChannelImageSlot, file: Blob & { name?: string }, upload: { wait?: boolean; edit?: Edit } = {}) => run(async () => {
      const result = await client.channels.uploadImage(need(), slot, file, upload);
      if (!upload.edit && typeof URL.createObjectURL === "function") {
        const url = URL.createObjectURL(file); blobs.current.push(url);
        setPreviews((current) => ({ client, key: previewKey, images: { ...(current.client === client && current.key === previewKey ? current.images : {}), [slot]: url } }));
      }
      return result;
    }),
  };
}

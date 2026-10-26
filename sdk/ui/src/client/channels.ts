import type { Channel, ChannelInput, ChannelUpdate, ChannelPage, MyChannelPage, ContentPage, EntityCard, ContentEntities, RefBody } from "./generated/wire.js";
import type { Http } from "./http.js";
import type { MediaClient, NamedOptions } from "./media/client.js";
import { call, type RouteQuery } from "./route.js";

export type ChannelQuery = RouteQuery<"GET", "/channels">;
export type MyChannelQuery = RouteQuery<"GET", "/me/channels">;
export type ChannelContentsQuery = RouteQuery<"GET", "/channels/{id}/contents">;
export type ChannelImageSlot = "cover" | "avatar";

/** Channels own posts and editorial teams; metadata entities only describe them. */
export class ChannelsClient {
  constructor(private readonly http: Http, private readonly media: MediaClient) {}

  list(query: ChannelQuery = {}, signal?: AbortSignal): Promise<ChannelPage> {
    return call(this.http, "GET", "/channels", { query, signal });
  }
  get(id: string, language?: string, signal?: AbortSignal): Promise<Channel> {
    return call(this.http, "GET", "/channels/{id}", { params: { id }, query: { language }, signal });
  }
  create(input: ChannelInput): Promise<Channel> {
    return this.write(call(this.http, "POST", "/channels", { body: input }));
  }
  update(id: string, patch: ChannelUpdate): Promise<Channel> {
    return this.write(call(this.http, "PATCH", "/channels/{id}", { params: { id }, body: patch }), id);
  }
  remove(id: string): Promise<void> {
    return this.write(call(this.http, "DELETE", "/channels/{id}", { params: { id } }), id);
  }
  mine(query: MyChannelQuery = {}, signal?: AbortSignal): Promise<MyChannelPage> {
    return call(this.http, "GET", "/me/channels", { query, signal });
  }
  contents(id: string, query: ChannelContentsQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/channels/{id}/contents", { params: { id }, query, signal });
  }
  async repost(id: string, contentId: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "PUT", "/channels/{id}/reposts/{content_id}", { params: { id, content_id: contentId } });
    emit({ type: "channel.reposts.changed", id });
  }
  async removeRepost(id: string, contentId: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/channels/{id}/reposts/{content_id}", { params: { id, content_id: contentId } });
    emit({ type: "channel.reposts.changed", id });
  }
  entities(id: string, language?: string, signal?: AbortSignal): Promise<EntityCard[]> {
    return call(this.http, "GET", "/channels/{id}/entities", { params: { id }, query: { language }, signal });
  }
  setEntities(id: string, set: ContentEntities, language?: string): Promise<EntityCard[]> {
    return this.write(call(this.http, "PUT", "/channels/{id}/entities", { params: { id }, query: { language }, body: set }), id);
  }
  folder(id: string): RefBody { return { kind: "channel", id }; }
  async uploadImage(id: string, slot: ChannelImageSlot, file: Blob & { name?: string }, options: Omit<NamedOptions, "path"> = {}): Promise<Channel> {
    const uploaded = await this.media.uploadNamed(this.folder(id), file, { ...options, path: `${slot}/${file.name || slot}` });
    return this.update(id, { [slot]: { image: uploaded.name } });
  }
  private async write<T>(promise: Promise<T>, id?: string): Promise<T> {
    const emit = this.http.captureChanges();
    const result = await promise;
    emit({ type: "channels.changed", id });
    return result;
  }
}

import { CatalogClient } from "./catalog.js";
import { CodesClient } from "./codes.js";
import { ContentsClient } from "./contents.js";
import { BansClient } from "./content/bans.js";
import { CommentsClient } from "./content/comments.js";
import { FavoritesClient } from "./content/favorites.js";
import { ModerationClient } from "./content/moderation.js";
import { PollsClient } from "./content/polls.js";
import { ReactionsClient } from "./content/reactions.js";
import type { Config, ContentModel, RefBody } from "./generated/wire.js";
import { DiscoveryClient } from "./discovery.js";
import { GroupsClient } from "./groups.js";
import { VersionsClient } from "./versions.js";
import { Http, type ContentKitChange, type ContentKitModule, type HttpOptions } from "./http.js";
import { MediaClient, type ContentFolders, type MediaOptions } from "./media/client.js";
import { call } from "./route.js";
import { NotificationsClient } from "./notifications.js";

export interface ContentKitClientOptions extends HttpOptions {
  /** Upload tuning (transport, part concurrency, retries). */
  media?: MediaOptions;
  /** The media kind of poll folders, when the host renamed it (content.Media). */
  folders?: ContentFolders;
}

/**
 * One ContentKit deployment as the browser sees it. Each module is a property
 * over one shared transport (mounts, auth, language, errors, change events).
 */
export interface ContentKitClient {
  /** Uploads, multipart, commits, reads, frames, HLS bases, hls.js `xhrSetup` and inline images. */
  readonly media: MediaClient;
  /** Content items: reads, and writes for their creators, owner-group editors and staff. */
  readonly contents: ContentsClient;
  /** The catalog's totals by kind, language and audience. */
  readonly catalog: CatalogClient;
  /** Threads, replies, edits, tombstones, reactions, standing, the latest feed, moderation. */
  readonly comments: CommentsClient;
  /** Likes and dislikes of an item. */
  readonly reactions: ReactionsClient;
  /** The signed-in caller's favorites. */
  readonly favorites: FavoritesClient;
  /** Polls: votes, free-text answers and the staff editor. */
  readonly polls: PollsClient;
  /** Comment bans, owner and global. */
  readonly bans: BansClient;
  /** The review queue of held comments and content. */
  readonly moderation: ModerationClient;
  /** Groups: listings, pages, staff writes and memberships. */
  readonly groups: GroupsClient;
  /** Lists, search, typeahead, rails, release dates, content filters, history, signals and exposures. */
  readonly discovery: DiscoveryClient;
  /** Content versions: reader plans, reader metadata, forks and downloads. */
  readonly versions: VersionsClient;
  /** Content-code lookup. */
  readonly codes: CodesClient;
  /** The signed-in person's native and imported notifications. */
  readonly notifications: NotificationsClient;

  /** What the content module allows: which interactions signed-out visitors may make. */
  config(signal?: AbortSignal): Promise<Config>;
  /** The site's content model: languages, tiers, kinds, group kinds and content filters. */
  kinds(signal?: AbortSignal): Promise<ContentModel>;
  /** A module's base URL as configured (no trailing slash). */
  url(module: ContentKitModule, ref?: RefBody): string;
  /** Cache context only, not authentication. The React provider keeps this in sync. */
  setReadScope(scope: string): void;
  /** Every successful mutation; a null scope means its data predates a context change. */
  subscribe(listener: (change: ContentKitChange, scope: string | null) => void): () => void;
}

export function createContentKitClient(options: ContentKitClientOptions): ContentKitClient {
  const { media: mediaOptions, folders, ...http } = options;
  const h = new Http(http);
  const media = new MediaClient(h, mediaOptions, folders);
  return {
    media,
    contents: new ContentsClient(h),
    catalog: new CatalogClient(h),
    comments: new CommentsClient(h),
    reactions: new ReactionsClient(h),
    favorites: new FavoritesClient(h),
    polls: new PollsClient(h, media),
    bans: new BansClient(h),
    moderation: new ModerationClient(h),
    groups: new GroupsClient(h, media),
    discovery: new DiscoveryClient(h),
    versions: new VersionsClient(h),
    codes: new CodesClient(h),
    notifications: new NotificationsClient(h),
    config: (signal) => call(h, "GET", "/config", { signal }),
    kinds: (signal) => call(h, "GET", "/kinds", { signal }),
    url: (module, ref) => h.mount(module, ref),
    setReadScope: (scope) => h.setReadScope(scope),
    subscribe: (listener) => h.subscribe(listener),
  };
}

import { CodesClient } from "./codes.js";
import { BansClient } from "./content/bans.js";
import { CommentsClient } from "./content/comments.js";
import { FavoritesClient } from "./content/favorites.js";
import { ModerationClient } from "./content/moderation.js";
import { PollsClient } from "./content/polls.js";
import { PostsClient } from "./content/posts.js";
import { ReactionsClient } from "./content/reactions.js";
import type { RefBody } from "./generated/wire.js";
import { Http, type ContentKitChange, type ContentKitModule, type HttpOptions } from "./http.js";
import { MediaClient, type ContentFolders, type MediaOptions } from "./media/client.js";
import { TaxonomyClient } from "./taxonomy.js";

export interface ContentKitClientOptions extends HttpOptions {
  /** Upload tuning (transport, part concurrency, retries). */
  media?: MediaOptions;
  /** The media kinds of post and poll folders, when the host renamed them (content.Media). */
  folders?: ContentFolders;
}

/**
 * One ContentKit deployment as the browser sees it. Each module is a property
 * over one shared transport (mounts, auth, language, errors, change events).
 */
export interface ContentKitClient {
  /** Uploads, multipart, commits, reads, frames, HLS bases, hls.js `xhrSetup` and inline images. */
  readonly media: MediaClient;
  /** Published posts, staff CRUD, reactions, cover and body images. */
  readonly posts: PostsClient;
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
  /** The review queue of held comments and posts. */
  readonly moderation: ModerationClient;
  /** The taxonomy admin API. */
  readonly taxonomy: TaxonomyClient;
  /** Content-code lookup. */
  readonly codes: CodesClient;

  /** A module's base URL as configured (no trailing slash). */
  url(module: ContentKitModule, ref?: RefBody): string;
  /** Every successful mutation made through this client; returns the unsubscribe. */
  subscribe(listener: (change: ContentKitChange) => void): () => void;
}

export function createContentKitClient(options: ContentKitClientOptions): ContentKitClient {
  const { media: mediaOptions, folders, ...http } = options;
  const h = new Http(http);
  const media = new MediaClient(h, mediaOptions, folders);
  return {
    media,
    posts: new PostsClient(h, media),
    comments: new CommentsClient(h),
    reactions: new ReactionsClient(h),
    favorites: new FavoritesClient(h),
    polls: new PollsClient(h, media),
    bans: new BansClient(h),
    moderation: new ModerationClient(h),
    taxonomy: new TaxonomyClient(h),
    codes: new CodesClient(h),
    url: (module, ref) => h.mount(module, ref),
    subscribe: (listener) => h.subscribe(listener),
  };
}

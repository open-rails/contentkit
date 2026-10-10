import type { RefBody } from "./generated/wire.js";
import { Http, type ContentKitChange, type ContentKitModule, type HttpOptions } from "./http.js";
import { MediaClient, type MediaOptions } from "./media/client.js";

export interface ContentKitClientOptions extends HttpOptions {
  /** Upload tuning (transport, part concurrency, retries). */
  media?: MediaOptions;
}

/**
 * One ContentKit deployment as the browser sees it. Each module is a property
 * over one shared transport (mounts, auth, language, errors, change events).
 */
export interface ContentKitClient {
  /** Uploads, multipart, commits, reads, frames, HLS bases and hls.js `xhrSetup`. */
  readonly media: MediaClient;
  // The social modules join here, each a class over the same Http:
  // posts, comments, reactions, favorites, polls, bans, moderation, taxonomy, codes.

  /** A module's base URL as configured (no trailing slash). */
  url(module: ContentKitModule, ref?: RefBody): string;
  /** Every successful mutation made through this client; returns the unsubscribe. */
  subscribe(listener: (change: ContentKitChange) => void): () => void;
}

export function createContentKitClient(options: ContentKitClientOptions): ContentKitClient {
  const { media, ...http } = options;
  const h = new Http(http);
  return {
    media: new MediaClient(h, media),
    url: (module, ref) => h.mount(module, ref),
    subscribe: (listener) => h.subscribe(listener),
  };
}

import { ContentKitError, readContentKitError, requestFailure } from "./errors.js";
import type { ContentChange } from "./content/types.js";
import { CONTENTKIT_MODULES } from "./generated/routes.js";
import type { FileInfo, Item, RefBody } from "./generated/wire.js";
import { browserReaderPreferences, type BrowserReaderPreferences, type GuestReaderPreferences } from "./browser-reader-preferences.js";

/** A module's base URL, or one per item (a host that splits uploads by kind). */
export type Mount = string | ((ref: RefBody) => string);

/**
 * Where each module lives when the host does not serve them all under
 * baseUrl's fixed sub-paths (CONTENTKIT_MODULES: the API version, then the
 * module's path). A mount is the module's full base URL.
 */
export interface ContentKitMounts {
  /** Comments, reactions, favorites, polls, bans, moderation. Default `{baseUrl}/v1`. */
  content?: string;
  /** Content items and catalog totals. Default `{baseUrl}/v1`. */
  contents?: string;
  /** Media reads and HLS playlists. Default `{baseUrl}/v1/media`. */
  media?: Mount;
  /** Uploads and commits. Default `{baseUrl}/v1/media/upload`. */
  upload?: Mount;
  /** Content-code lookup. Default `{baseUrl}/v1/codes`. */
  codes?: string;
  /** Entities. Default `{baseUrl}/v1`. */
  entities?: string;
  /** The site's content model. Default `{baseUrl}/v1`. */
  kinds?: string;
  /** User inbox. Default `{baseUrl}/v1/notifications`. */
  notifications?: string;
  /** Content versions: reader plans, reader metadata, forks, downloads. Default `{baseUrl}/v1`. */
  versions?: string;
  /** Discovery: lists, search, typeahead, similar, recommendations, history, signals. Default `{baseUrl}/v1`. */
  discovery?: string;
  /** Bulk import (a service's). Default `{baseUrl}/v1`. */
  import?: string;
  /** Durable archive publishing operations. Default `{baseUrl}/v1`. */
  publishing?: string;
}

export type ContentKitModule = keyof ContentKitMounts;

/** A mutation made through the client: the host's cache-invalidation hook gets each one. */
export type ContentKitChange =
  /** A commit applied; files are the item's uploads as an editor reads them, without URLs. */
  | { type: "media.committed"; ref: RefBody; files: FileInfo[] }
  /** An upload finished processing, as an editor read returns it. */
  | { type: "media.processed"; ref: RefBody; file: FileInfo }
  | { type: "notifications.changed" }
  | { type: "notifications.updated"; item: Item }
  | ContentChange;

export type TokenSource = () => string | null | undefined | Promise<string | null | undefined>;

export interface HttpOptions {
  /** Where the host mounts ContentKit's routes (contentkit.Routes.Prefix), e.g. "/api/contentkit". */
  baseUrl: string;
  mounts?: ContentKitMounts;
  /** Transport for API calls; pass an authenticating fetch (auth-ui's authFetch). */
  fetch?: typeof fetch;
  /** Bearer for API calls and HLS playlists. An authenticating fetch overrides it. */
  token?: TokenSource;
  /** Sent as Accept-Language. */
  language?: () => string | null | undefined;
  /** Extra headers per call (CSRF). */
  headers?: () => HeadersInit | Promise<HeadersInit>;
  /** Default "same-origin"; "include" also sends cookies with HLS segment requests. */
  credentials?: RequestCredentials;
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
  /** Read the body as a Blob instead of JSON. */
  blob?: boolean;
  keepalive?: boolean;
  /** Internal preference inventory reads never send unvalidated overrides. */
  guestPreferences?: boolean;
  /** Internal: avoid repeating recovery if a refreshed override is rejected. */
  retryGuestPreferences?: boolean;
}

const trim = (u: string) => u.replace(/\/+$/, "");

/** How a player loads HLS: the media client's `hls`. */
export interface HlsLoader {
  /** Fetches a playlist or sprite VTT with the app's credentials. */
  playlist(url: string, signal?: AbortSignal): Promise<Response>;
  /** hls.js `xhrSetup` for segments: cookies, never the app's credentials. */
  segment(xhr: XMLHttpRequest, url: string): void;
}

/** The transport every module shares: mounts, auth, language, errors and change events. */
export class Http {
  private readonly listeners = new Set<(change: ContentKitChange, scope: string | null) => void>();
  private readScope = { key: "{}" };
  private readonly base: string;
  readonly browserPreferences: BrowserReaderPreferences;
  private viewer?: string | null;
  private viewerRevision = 0;
  private filterIDs?: Set<string>;
  private filterInventory?: Promise<void>;

  constructor(private readonly o: HttpOptions) {
    if (typeof o.baseUrl !== "string") throw new Error("contentkit: createContentKitClient needs baseUrl");
    this.base = trim(o.baseUrl);
    this.browserPreferences = browserReaderPreferences(this.mount("discovery"));
  }

  setViewer(viewer: string | null | undefined): void {
    if (viewer !== this.viewer) this.viewerRevision++;
    this.viewer = viewer;
  }

  acceptGuestFilters(filters: readonly { entity: { id: string } }[]): void {
    this.filterIDs = new Set(filters.map((row) => row.entity.id));
  }
  private guestFilters(saved = this.browserPreferences.snapshot()): string | null {
    const filters = saved.filters.filter((row) => this.filterIDs?.has(row.entity_id));
    if (!filters.length) return null;
    const encoded = JSON.stringify({ filters });
    if (filters.length > 128 || new TextEncoder().encode(encoded).length > 8192)
      throw new ContentKitError(
        "invalid_request",
        "Too many content filter preferences. Your previous choices have been kept.",
      );
    return encoded;
  }
  validateGuestPreferences(value: GuestReaderPreferences): void {
    if (!this.filterIDs) throw new ContentKitError("invalid_request", "Content filters have not loaded.");
    this.guestFilters(value);
  }
  private async validateGuestFilters(): Promise<void> {
    const saved = this.browserPreferences.snapshot();
    if (this.filterIDs || !saved.filters.length) return;
    this.filterInventory ??= this.request<{ filters: { entity: { id: string } }[] }>(
      `${this.mount("discovery")}/content-filters`,
      { guestPreferences: false },
    )
      .then((result) => this.acceptGuestFilters(result.filters))
      .finally(() => {
        this.filterInventory = undefined;
      });
    await this.filterInventory;
  }

  /** Current request language for APIs that also bind it into their body. */
  language(): string | undefined {
    return this.o.language?.() ?? undefined;
  }

  /** The module's base URL, without a trailing slash. */
  mount(module: ContentKitModule, ref?: RefBody): string {
    const m = this.o.mounts?.[module];
    if (typeof m === "function") {
      if (!ref) throw new Error(`contentkit: the ${module} mount is per item; pass a ref`);
      return trim(m(ref));
    }
    if (m !== undefined) return trim(m);
    return this.base + CONTENTKIT_MODULES[module];
  }

  async request<T>(
    url: string,
    {
      method,
      body,
      signal,
      blob,
      keepalive,
      guestPreferences = true,
      retryGuestPreferences = true,
    }: RequestOptions = {},
  ): Promise<T> {
    const viewerRevision = this.viewerRevision;
    if (this.viewer === null && guestPreferences) await this.validateGuestFilters();
    if (viewerRevision !== this.viewerRevision) throw new ContentKitError("aborted", "The account changed.");
    const f = this.o.fetch ?? fetch;
    const headers = new Headers(await this.o.headers?.());
    headers.delete("ContentKit-Reader-Preferences");
    if (body !== undefined) headers.set("Content-Type", "application/json");
    const token = await this.o.token?.();
    if (token && !headers.has("Authorization")) headers.set("Authorization", `Bearer ${token}`);
    const lang = this.o.language?.();
    if (lang) headers.set("Accept-Language", lang);
    if (this.viewer === null && guestPreferences) {
      const filters = this.guestFilters();
      if (filters) headers.set("ContentKit-Reader-Preferences", filters);
    }
    if (viewerRevision !== this.viewerRevision) throw new ContentKitError("aborted", "The account changed.");
    let res: Response;
    try {
      res = await f(url, {
        method: method ?? (body === undefined ? "GET" : "POST"),
        headers,
        body: body === undefined ? null : JSON.stringify(body),
        credentials: this.o.credentials ?? "same-origin",
        signal: signal ?? null,
        ...(keepalive ? { keepalive } : {}),
      });
    } catch (err) {
      throw requestFailure(err, signal, `ContentKit API ${new URL(url, "http://x").pathname}`);
    }
    if (!res.ok) {
      const error = await readContentKitError(res);
      const old = headers.get("ContentKit-Reader-Preferences");
      if (
        (method ?? (body === undefined ? "GET" : "POST")) === "GET" &&
        error.code === "invalid_request" &&
        old &&
        this.viewer === null &&
        retryGuestPreferences
      ) {
        this.filterIDs = undefined;
        await this.validateGuestFilters();
        if (viewerRevision !== this.viewerRevision) throw new ContentKitError("aborted", "The account changed.");
        const current = this.guestFilters();
        if (current !== old)
          return this.request(url, {
            method,
            body,
            signal,
            blob,
            keepalive,
            guestPreferences,
            retryGuestPreferences: false,
          });
      }
      throw error;
    }
    if (blob) return (await res.blob()) as T;
    return (res.status === 204 ? undefined : await res.json()) as T;
  }

  /**
   * How players load this API's HLS. Playlists and sprite VTTs go through
   * the client's fetch, so they carry what its other calls carry (a DPoP
   * proof when that fetch adds one); segments only carry cookies.
   */
  readonly hls: HlsLoader = {
    playlist: async (url, signal) => {
      const init: RequestInit = { credentials: this.o.credentials ?? "same-origin", signal: signal ?? null };
      if (!this.onApiOrigin(url)) return fetch(url, init);
      const token = await this.o.token?.();
      return (this.o.fetch ?? fetch)(url, token ? { ...init, headers: { Authorization: `Bearer ${token}` } } : init);
    },
    segment: (xhr, url) => {
      if (!xhr.readyState) xhr.open("GET", url, true);
      if (this.o.credentials === "include") xhr.withCredentials = true;
    },
  };

  setReadScope(key: string): void {
    if (key !== this.readScope.key) this.readScope = { key };
  }

  /** Capture before awaiting a write; null tells caches to refetch after a scope change. */
  captureChanges(): (change: ContentKitChange) => void {
    const scope = this.readScope;
    return (change) => this.emit(change, scope === this.readScope ? scope.key : null);
  }

  subscribe(listener: (change: ContentKitChange, scope: string | null) => void): () => void {
    this.listeners.add(listener);
    return () => void this.listeners.delete(listener);
  }

  private emit(change: ContentKitChange, scope: string | null): void {
    for (const l of this.listeners) {
      try {
        l(change, scope);
      } catch (e) {
        // A host listener's fault must not fail the mutation that succeeded.
        console.error("contentkit: change listener failed", e);
      }
    }
  }

  private onApiOrigin(url: string): boolean {
    const here = typeof location === "undefined" ? "http://localhost" : location.href;
    try {
      return new URL(url, here).origin === new URL(this.base || "/", here).origin;
    } catch {
      return false;
    }
  }
}

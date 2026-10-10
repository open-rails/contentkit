import { ContentKitError, aborted, readContentKitError } from "./errors.js";
import type { FileInfo, RefBody } from "./generated/wire.js";

/** A module's base URL, or one per item (a host that splits uploads by kind). */
export type Mount = string | ((ref: RefBody) => string);

/**
 * Where each module lives when the host does not serve them all under
 * baseUrl's fixed sub-paths.
 */
export interface ContentKitMounts {
  /** Posts, comments, reactions, favorites, polls, bans, moderation. Default baseUrl. */
  content?: string;
  /** Media reads and HLS playlists. Default `{baseUrl}/media`. */
  media?: Mount;
  /** Uploads and commits. Default `{baseUrl}/media/upload`. */
  upload?: Mount;
  /** Content-code lookup. Default `{baseUrl}/codes`. */
  codes?: string;
  /** Taxonomy. Default `{baseUrl}/taxonomy`. */
  taxonomy?: string;
}

export type ContentKitModule = keyof ContentKitMounts;

/** A mutation made through the client: the host's cache-invalidation hook gets each one. */
export type ContentKitChange =
  /** A commit applied; files are the item's uploads as an editor reads them, without URLs. */
  | { type: "media.committed"; ref: RefBody; files: FileInfo[] }
  /** An upload finished processing, as an editor read returns it. */
  | { type: "media.processed"; ref: RefBody; file: FileInfo };

export type TokenSource = () => string | null | undefined | Promise<string | null | undefined>;

export interface HttpOptions {
  /** Where the host mounts ContentKit, e.g. "/api/v1/contentkit". */
  baseUrl: string;
  mounts?: ContentKitMounts;
  /** Transport for API calls; pass an authenticating fetch (auth-ui's authFetch). */
  fetch?: typeof fetch;
  /**
   * Bearer for API calls and for HLS playlists on the API origin (hls.js
   * loads them with XHR, not fetch). An authenticating fetch overrides it.
   */
  token?: TokenSource;
  /** Sent as Accept-Language. */
  language?: () => string | null | undefined;
  /** Extra headers per call (CSRF). */
  headers?: () => HeadersInit | Promise<HeadersInit>;
  /** Default "same-origin"; "include" also sends cookies with HLS requests. */
  credentials?: RequestCredentials;
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
  /** Read the body as a Blob instead of JSON. */
  blob?: boolean;
}

const trim = (u: string) => u.replace(/\/+$/, "");

/** The transport every module shares: mounts, auth, language, errors and change events. */
export class Http {
  private readonly listeners = new Set<(change: ContentKitChange) => void>();
  private readonly base: string;

  constructor(private readonly o: HttpOptions) {
    if (typeof o.baseUrl !== "string") throw new Error("contentkit: createContentKitClient needs baseUrl");
    this.base = trim(o.baseUrl);
  }

  /** The module's base URL, without a trailing slash. */
  mount(module: ContentKitModule, ref?: RefBody): string {
    const m = this.o.mounts?.[module];
    if (typeof m === "function") {
      if (!ref) throw new Error(`contentkit: the ${module} mount is per item; pass a ref`);
      return trim(m(ref));
    }
    if (m !== undefined) return trim(m);
    switch (module) {
      case "content":
        return this.base;
      case "media":
        return `${this.base}/media`;
      case "upload":
        return `${this.base}/media/upload`;
      default:
        return `${this.base}/${module}`;
    }
  }

  async request<T>(url: string, { method, body, signal, blob }: RequestOptions = {}): Promise<T> {
    const f = this.o.fetch ?? fetch;
    const headers = new Headers(await this.o.headers?.());
    if (body !== undefined) headers.set("Content-Type", "application/json");
    const token = await this.o.token?.();
    if (token && !headers.has("Authorization")) headers.set("Authorization", `Bearer ${token}`);
    const lang = this.o.language?.();
    if (lang) headers.set("Accept-Language", lang);
    let res: Response;
    try {
      res = await f(url, {
        method: method ?? (body === undefined ? "GET" : "POST"),
        headers,
        body: body === undefined ? null : JSON.stringify(body),
        credentials: this.o.credentials ?? "same-origin",
        signal: signal ?? null,
      });
    } catch (err) {
      if (signal?.aborted) throw aborted(signal);
      throw new ContentKitError("network", `ContentKit API ${new URL(url, "http://x").pathname} unreachable`, { cause: err });
    }
    if (!res.ok) throw await readContentKitError(res);
    if (blob) return (await res.blob()) as T;
    return (res.status === 204 ? undefined : await res.json()) as T;
  }

  /**
   * hls.js `xhrSetup`: opens the request, sends cookies when credentials is
   * "include", and adds the bearer only on the API's origin (never a media host).
   */
  readonly xhrSetup = async (xhr: XMLHttpRequest, url: string): Promise<void> => {
    if (!xhr.readyState) xhr.open("GET", url, true);
    if (this.o.credentials === "include") xhr.withCredentials = true;
    if (!this.onApiOrigin(url)) return;
    const token = await this.o.token?.();
    if (token) xhr.setRequestHeader("Authorization", `Bearer ${token}`);
  };

  subscribe(listener: (change: ContentKitChange) => void): () => void {
    this.listeners.add(listener);
    return () => void this.listeners.delete(listener);
  }

  emit(change: ContentKitChange): void {
    for (const l of this.listeners) {
      try {
        l(change);
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

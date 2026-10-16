import type { FileDownload, ForkBody, PageReview, PageReviewPatch, Pending, ReaderMeta, ReaderPlan } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, routeURL } from "./route.js";

/** A version's answer while it is prepared (202). */
export const isPending = (x: unknown): x is Pending => !!x && typeof x === "object" && "status" in x && !("pages" in x) && !("url" in x);

/** Content versions: page review, reader plans, reader metadata, forks and downloads. */
export class VersionsClient {
  constructor(private readonly http: Http) {}

  /** Editors: retained page sources, automatic decisions and manual overrides. */
  pages(content: string, version: string, signal?: AbortSignal): Promise<PageReview> {
    return call(this.http, "GET", "/contents/{id}/versions/{version}/pages", { params: { id: content, version }, signal });
  }
  /** Editors: changes choices or order against the review's current revision. */
  async updatePages(content: string, version: string, patch: PageReviewPatch): Promise<PageReview> {
    const emit = this.http.captureChanges();
    const next: PageReview = await call(this.http, "PATCH", "/contents/{id}/versions/{version}/pages", { params: { id: content, version }, body: patch });
    emit({ type: "content.changed", id: content });
    emit({ type: "reader.changed", content, version });
    return next;
  }
  /** The version's reader plan for this viewer; Pending while it has no current publication. */
  reader(content: string, version: string, signal?: AbortSignal): Promise<ReaderPlan | Pending> {
    return call(this.http, "GET", "/contents/{id}/versions/{version}/reader", { params: { id: content, version }, signal });
  }
  /** Editors: replaces the version's reader metadata (anchors, audio cues, defaults). */
  async setReader(content: string, version: string, meta: ReaderMeta): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "PUT", "/contents/{id}/versions/{version}/reader", { params: { id: content, version }, body: meta });
    emit({ type: "reader.changed", content, version });
  }
  /** Editors: fills a version without media from a sibling, in a durable job. */
  fork(content: string, version: string, body: ForkBody): Promise<Pending> {
    return call(this.http, "POST", "/contents/{id}/versions/{version}/fork", { params: { id: content, version }, body });
  }
  /**
   * Grants the version's download, what its kind serves (the original, a
   * rendition, its pages as a zip), charged to the viewer's download quota:
   * `url` (absolute) serves it as an attachment until `expires_at`. Pending
   * while it is prepared (ask again).
   */
  async download(content: string, version: string, signal?: AbortSignal): Promise<FileDownload | Pending> {
    const url = routeURL(this.http, "GET", "/contents/{id}/versions/{version}/download", { params: { id: content, version } });
    const out = await this.http.request<FileDownload | Pending>(url, { signal });
    if (isPending(out)) return out;
    // A marked zip's URL is the archive route's, relative to this one.
    const base = new URL(url, globalThis.location?.href ?? "http://localhost/");
    return { ...out, url: new URL(out.url, base).toString() };
  }
}

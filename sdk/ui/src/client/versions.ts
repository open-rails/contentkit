import { ContentKitError } from "./errors.js";
import type { ForkBody, Pending, ReaderMeta, ReaderPlan } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, routeURL } from "./route.js";

/** A version's answer while it is prepared (202). */
export const isPending = (x: unknown): x is Pending => !!x && typeof x === "object" && "status" in x && !("pages" in x);

/** Content versions: reader plans, reader metadata, forks and downloads. */
export class VersionsClient {
  constructor(private readonly http: Http) {}

  /** The version's reader plan for this viewer; Pending while it has no current publication. */
  reader(content: string, version: string, signal?: AbortSignal): Promise<ReaderPlan | Pending> {
    return call(this.http, "GET", "/contents/{id}/versions/{version}/reader", { params: { id: content, version }, signal });
  }
  /** Editors: the version's anchors and audio cues. */
  setReader(content: string, version: string, meta: ReaderMeta): Promise<void> {
    return call(this.http, "PUT", "/contents/{id}/versions/{version}/reader", { params: { id: content, version }, body: meta });
  }
  /** Editors: fills a version without media from a sibling, in a durable job. */
  fork(content: string, version: string, body: ForkBody): Promise<Pending> {
    return call(this.http, "POST", "/contents/{id}/versions/{version}/fork", { params: { id: content, version }, body });
  }
  /**
   * The version's pages as a zip, charged to the viewer's download quota;
   * Pending while it is prepared (ask again).
   */
  async download(content: string, version: string, signal?: AbortSignal): Promise<Blob | Pending> {
    const url = routeURL(this.http, "GET", "/contents/{id}/versions/{version}/download", { params: { id: content, version } });
    const out = await this.http.request<Blob>(url, { signal, blob: true });
    if (!out.type.startsWith("application/json")) return out;
    const body: unknown = JSON.parse(await out.text());
    if (isPending(body)) return body;
    throw new ContentKitError("internal_error", "unexpected download answer");
  }
}

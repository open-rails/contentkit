import type {
  CatalogText,
  Content,
  ContentInput,
  ContentPatch,
  Creator,
  InlineImage,
  ManageCount,
  Segment,
  Version,
  VersionInput,
} from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, type RouteQuery } from "./route.js";

/** The URL scheme of an image reference in a content body. */
export const IMAGE_SCHEME = "contentkit:";

/**
 * The reference a body stores for one of its images, by name
 * ("contentkit:i-{uuid}"), where the image's URL would go: reads resolve it to
 * the image's current public file (`Content.images`).
 */
export const imageRef = (name: string) => IMAGE_SCHEME + name;

/** Matches every image reference; the name is group 1. */
export const imageRefs = () => /contentkit:(i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/g;

/** Management filters; kind selects /manage/{kind}, otherwise /manage. */
export type ManageQuery = RouteQuery<"GET", "/manage"> & { kind?: string };

/** The /manage/{kind} filters a count takes. */
export type ManageCountQuery = RouteQuery<"GET", "/manage/{kind}/count"> & { kind: string };

/**
 * Content items: a viewer's reads, and the writes of their creators, their
 * owner group's editors and root:content:write holders.
 */
export class ContentsClient {
  constructor(private readonly http: Http) {}

  /** The item as the caller sees it; text in language, else the site's first, else any. */
  get(id: string, language?: string, signal?: AbortSignal): Promise<Content> {
    return call(this.http, "GET", "/contents/{id}", { params: { id }, query: { language }, signal });
  }
  version(id: string, vid: string, language?: string, signal?: AbortSignal): Promise<Version> {
    return call(this.http, "GET", "/contents/{id}/versions/{vid}", { params: { id, vid }, query: { language }, signal });
  }
  /** Items the caller manages, newest first: all for root:content:write holders, else theirs or an owner group's. */
  manage(q: ManageQuery = {}, signal?: AbortSignal): Promise<Content[]> {
    const { kind, ...query } = q;
    return kind
      ? call(this.http, "GET", "/manage/{kind}", { params: { kind }, query, signal })
      : call(this.http, "GET", "/manage", { query, signal });
  }
  /** How many items of a kind the caller manages under these filters: exact up to 10,000, then truncated. */
  manageCount(q: ManageCountQuery, signal?: AbortSignal): Promise<ManageCount> {
    const { kind, ...query } = q;
    return call(this.http, "GET", "/manage/{kind}/count", { params: { kind }, query, signal });
  }
  /** Creates a draft. */
  create(input: ContentInput): Promise<Content> {
    return this.write(call(this.http, "POST", "/contents", { body: input }));
  }
  update(id: string, patch: ContentPatch): Promise<Content> {
    return this.write(call(this.http, "PATCH", "/contents/{id}", { params: { id }, body: patch }));
  }
  /** Sets the text in one language; moderation "held" when the moderator holds the edit. */
  putText(id: string, language: string, text: CatalogText): Promise<Content> {
    return this.write(call(this.http, "PUT", "/contents/{id}/text/{language}", { params: { id, language }, body: text }));
  }
  deleteText(id: string, language: string): Promise<Content> {
    return this.write(call(this.http, "DELETE", "/contents/{id}/text/{language}", { params: { id, language } }));
  }
  setCreators(id: string, creators: Creator[]): Promise<Content> {
    return this.write(call(this.http, "PUT", "/contents/{id}/creators", { params: { id }, body: { creators } }));
  }
  /** Publishes now, or schedules for at; moderation "held" when the moderator holds it. */
  publish(id: string, at?: string): Promise<Content> {
    return this.write(call(this.http, "POST", "/contents/{id}/publish", { params: { id }, body: { publish_at: at } }));
  }
  unpublish(id: string): Promise<Content> {
    return this.write(call(this.http, "POST", "/contents/{id}/unpublish", { params: { id } }));
  }
  async remove(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/contents/{id}", { params: { id } });
    emit({ type: "content.deleted", id });
  }
  restore(id: string): Promise<Content> {
    return this.write(call(this.http, "POST", "/contents/{id}/restore", { params: { id } }));
  }
  /** Selects a version's cover by the name ("i-{uuid}") of one of its cover uploads; null returns to its first. */
  setCover(id: string, vid: string, image: string | null): Promise<Version> {
    return this.updateVersion(id, vid, { cover: image ?? "" });
  }
  /**
   * A body image uploaded to version vid (its private files), copied into
   * every other version: the reference to store in the body and a URL that shows
   * it to the editor now (null while none exists).
   */
  inlineImage(id: string, vid: string, image: string, signal?: AbortSignal): Promise<InlineImage> {
    return call(this.http, "POST", "/contents/{id}/images", { params: { id }, body: { image, version: vid }, signal });
  }
  addVersion(id: string, input: VersionInput): Promise<Version> {
    return this.writeVersion(id, call(this.http, "POST", "/contents/{id}/versions", { params: { id }, body: input }));
  }
  updateVersion(id: string, vid: string, input: VersionInput): Promise<Version> {
    return this.writeVersion(id, call(this.http, "PATCH", "/contents/{id}/versions/{vid}", { params: { id, vid }, body: input }));
  }
  async removeVersion(id: string, vid: string): Promise<void> {
    await this.writeVersion(id, call(this.http, "DELETE", "/contents/{id}/versions/{vid}", { params: { id, vid } }));
  }
  /** Orders the versions: ids lists each version that is not deleted, first first. */
  orderVersions(id: string, ids: string[]): Promise<Content> {
    return this.write(call(this.http, "PUT", "/contents/{id}/versions/order", { params: { id }, body: { ids } }));
  }
  /** Replaces a version's segments (chapters). */
  setSegments(id: string, vid: string, segments: Segment[]): Promise<Version> {
    return this.writeVersion(id, call(this.http, "PUT", "/contents/{id}/versions/{vid}/segments", { params: { id, vid }, body: { segments } }));
  }

  private async write(p: Promise<Content>): Promise<Content> {
    const emit = this.http.captureChanges();
    const content = await p;
    emit({ type: "content.saved", content });
    return content;
  }

  private async writeVersion<T>(id: string, p: Promise<T>): Promise<T> {
    const emit = this.http.captureChanges();
    const out = await p;
    emit({ type: "content.changed", id });
    return out;
  }
}

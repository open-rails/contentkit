import type { InlineImage, Post, PostCover, PostInput } from "../generated/wire.js";
import type { Http } from "../http.js";
import type { MediaClient, NamedOptions } from "../media/client.js";
import { call, type PageQuery } from "../route.js";
import { reactionVerb, type Reaction, type Sort } from "./types.js";

export interface PostQuery extends PageQuery {
  language?: string;
  sort?: Sort;
}

export interface PostAdminQuery extends PageQuery {
  language?: string;
  /** true: only drafts; false: only posts that are not drafts. */
  draft?: boolean;
  /** true: only deleted posts (with deleted_at); otherwise only live ones. */
  deleted?: boolean;
  /** Only posts whose title, excerpt or body contains this text, ignoring case. */
  q?: string;
}

/** Posts: the published list, staff CRUD, search and restore, reactions, the cover and body images. */
export class PostsClient {
  constructor(
    private readonly http: Http,
    private readonly media: MediaClient,
  ) {}

  /** Published posts. */
  list(q: PostQuery = {}, signal?: AbortSignal): Promise<Post[]> {
    return call(this.http, "GET", "/posts", { query: q, signal });
  }
  /** Every post for staff: drafts, scheduled, held and rejected ones included; deleted ones on their own; q searches (PostWrite). */
  adminList(q: PostAdminQuery = {}, signal?: AbortSignal): Promise<Post[]> {
    return call(this.http, "GET", "/posts/admin", { query: q, signal });
  }
  get(id: string, signal?: AbortSignal): Promise<Post> {
    return call(this.http, "GET", "/posts/{id}", { params: { id }, signal });
  }
  /** Creates a post (PostWrite); moderation "held" when the moderator holds it. */
  async create(input: PostInput): Promise<Post> {
    const emit = this.http.captureChanges();
    const post: Post = await call(this.http, "POST", "/posts", { body: input });
    emit({ type: "post.created", post });
    return post;
  }
  /** Updates the given fields (PostWrite). */
  async update(id: string, input: PostInput): Promise<Post> {
    const emit = this.http.captureChanges();
    const post: Post = await call(this.http, "PATCH", "/posts/{id}", { params: { id }, body: input });
    emit({ type: "post.updated", post });
    return post;
  }
  async delete(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/posts/{id}", { params: { id } });
    emit({ type: "post.deleted", id });
  }
  /** Restores a deleted post as it was (PostWrite); conflict when a live post took its slug. */
  async restore(id: string): Promise<Post> {
    const emit = this.http.captureChanges();
    const post: Post = await call(this.http, "POST", "/posts/{id}/restore", { params: { id } });
    emit({ type: "post.restored", post });
    return post;
  }
  /** Sets the caller's reaction to a published post; resolves with the post's new totals. */
  async react(id: string, value: Reaction): Promise<Post> {
    const emit = this.http.captureChanges();
    const post: Post = await call(this.http, "POST", `/posts/{id}/${reactionVerb(value)}` as const, { params: { id } });
    emit({ type: "post.updated", post });
    return post;
  }
  /** Sets the cover to an inline image name ("i-{uuid}") of the post's folder, or clears it; resolves with its URL. */
  async setCover(id: string, image: string | null): Promise<string | null> {
    const emit = this.http.captureChanges();
    const r: PostCover = await call(this.http, "PUT", "/posts/{id}/cover", { params: { id }, body: { image: image ?? "" } });
    emit({ type: "post.changed", id });
    return r.cover_url;
  }
  /** Uploads file to the post's folder and makes it the cover (null clears it); resolves with its URL. */
  async uploadCover(id: string, file: Blob | null, o?: NamedOptions): Promise<string | null> {
    if (!file) return this.setCover(id, null);
    const up = await this.media.uploadNamed(this.media.postRef(id), file, o);
    return this.setCover(id, up.name);
  }
  /** The public URL of an inline image of the post's folder, to place in its body. */
  async imageURL(id: string, image: string, signal?: AbortSignal): Promise<string> {
    const r: InlineImage = await call(this.http, "POST", "/posts/{id}/images", { params: { id }, body: { image }, signal });
    return r.url;
  }
}

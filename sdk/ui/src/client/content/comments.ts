import type { AdminComment, Comment, CommentInput, CommentStanding, FeedItem, ReactionCounts } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call, type PageQuery } from "../route.js";
import { reactionVerb, type Reaction, type Sort, type Target } from "./types.js";

export interface CommentQuery extends PageQuery {
  sort?: Sort;
  /** Only this language's thread. */
  language?: string;
}

export interface AdminCommentQuery extends PageQuery {
  /** Only comments on this content kind. */
  contentKind?: string;
  /** Only comments by this user id. */
  author?: string;
}

/** Comments on an item: threads, replies, edits, tombstones, reactions, standing, the latest feed and moderation. */
export class CommentsClient {
  constructor(private readonly http: Http) {}

  /** The target's top-level comments with reply counts; the caller also gets its own held and rejected ones. */
  list(t: Target, q: CommentQuery = {}, signal?: AbortSignal): Promise<Comment[]> {
    const o = { params: { id: t.id }, signal };
    return t.type === "entities"
      ? call(this.http, "GET", "/entities/{id}/comments", { ...o, query: q })
      : call(this.http, "GET", "/contents/{id}/comments", { ...o, query: { ...q, version: t.version } });
  }
  /** A top-level comment's replies, oldest first. */
  replies(id: string, q: PageQuery = {}, signal?: AbortSignal): Promise<Comment[]> {
    return call(this.http, "GET", "/comments/{cid}/replies", { params: { cid: id }, query: q, signal });
  }
  /** Comments on the target in a language's thread, or replies to a top-level comment (reply_to_id); anonymous callers give anon_name. */
  async create(t: Target, input: CommentInput): Promise<Comment> {
    const emit = this.http.captureChanges();
    const o = { params: { id: t.id }, body: input };
    const comment: Comment =
      t.type === "entities" ? await call(this.http, "POST", "/entities/{id}/comments", o) : await call(this.http, "POST", "/contents/{id}/comments", { ...o, query: { version: t.version } });
    emit({ type: "comment.created", target: t, comment });
    return comment;
  }
  /** Replaces the body: its author, or a moderator. */
  async edit(id: string, body: string): Promise<Comment> {
    const emit = this.http.captureChanges();
    const comment: Comment = await call(this.http, "PATCH", "/comments/{cid}", { params: { cid: id }, body: { body } });
    emit({ type: "comment.updated", comment });
    return comment;
  }
  /** Leaves a tombstone: its author, or a moderator. */
  async delete(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/comments/{cid}", { params: { cid: id } });
    emit({ type: "comment.deleted", id });
  }
  /** Sets the caller's reaction to a comment. */
  async react(id: string, value: Reaction): Promise<ReactionCounts> {
    const emit = this.http.captureChanges();
    const params = { cid: id };
    const counts: ReactionCounts =
      value === 0 ? await call(this.http, "DELETE", "/comments/{cid}/reaction", { params }) : await call(this.http, "POST", `/comments/{cid}/${reactionVerb(value)}` as const, { params });
    emit({ type: "comment.reacted", id, counts });
    return counts;
  }
  /** Commenting permissions and the authoritative visible top-level count in query.language (all languages by default); hidden/locked threads withhold the count. */
  standing(t: Target, signal?: AbortSignal, query: { language?: string } = {}): Promise<CommentStanding> {
    const o = { params: { id: t.id }, signal, query };
    return t.type === "entities"
      ? call(this.http, "GET", "/entities/{id}/can-comment", o)
      : call(this.http, "GET", "/contents/{id}/can-comment", { ...o, query: { ...query, version: t.version } });
  }
  /** The newest published comments across the site, with their items; a page may under-fill. */
  latest(q: PageQuery & { language?: string } = {}, signal?: AbortSignal): Promise<FeedItem[]> {
    return call(this.http, "GET", "/comments/latest", { query: q, signal });
  }
  /** Every comment, newest first, deleted, held and rejected ones with their bodies (root:comments:moderate). */
  adminList(q: AdminCommentQuery = {}, signal?: AbortSignal): Promise<AdminComment[]> {
    const { contentKind, ...rest } = q;
    return call(this.http, "GET", "/comments/admin", { query: { ...rest, content_kind: contentKind }, signal });
  }
  /** Restores a deleted comment (root:comments:moderate). */
  async restore(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "POST", "/comments/{cid}/restore", { params: { cid: id } });
    emit({ type: "comment.restored", id });
  }
}

import type { AdminComment, Comment, CommentInput, CommentStanding, FeedItem, ReactionCounts, RefBody } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call, type PageQuery } from "../route.js";
import { reactionVerb, type Reaction, type Sort } from "./types.js";

export interface CommentQuery extends PageQuery {
  sort?: Sort;
}

export interface AdminCommentQuery extends PageQuery {
  /** Only comments on this content kind. */
  contentKind?: string;
}

// Content ids are the host's opaque text (not only media's UUIDv7s).
const target = (ref: RefBody) => ({ kind: ref.kind, id: ref.id });

/** Comments on an item: threads, replies, edits, tombstones, reactions, standing, the latest feed and moderation. */
export class CommentsClient {
  constructor(private readonly http: Http) {}

  /** The item's top-level comments with reply counts; the caller also gets its own held and rejected ones. */
  list(ref: RefBody, q: CommentQuery = {}, signal?: AbortSignal): Promise<Comment[]> {
    return call(this.http, "GET", "/{kind}/{id}/comments", { params: target(ref), query: q, signal });
  }
  /** A top-level comment's replies, oldest first. */
  replies(id: string, q: PageQuery = {}, signal?: AbortSignal): Promise<Comment[]> {
    return call(this.http, "GET", "/comments/{cid}/replies", { params: { cid: id }, query: q, signal });
  }
  /** Comments on the item, or replies to a top-level comment (reply_to_id); anonymous callers give anon_name. */
  async create(ref: RefBody, input: CommentInput): Promise<Comment> {
    const emit = this.http.captureChanges();
    const comment: Comment = await call(this.http, "POST", "/{kind}/{id}/comments", { params: target(ref), body: input });
    emit({ type: "comment.created", ref: target(ref), comment });
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
    const counts: ReactionCounts = await call(this.http, "POST", `/comments/{cid}/${reactionVerb(value)}` as const, { params: { cid: id } });
    emit({ type: "comment.reacted", id, counts });
    return counts;
  }
  /** Whether the caller may comment on the item, the ban that stops it, and what it may do to others' comments. */
  standing(ref: RefBody, signal?: AbortSignal): Promise<CommentStanding> {
    return call(this.http, "GET", "/{kind}/{id}/can-comment", { params: target(ref), signal });
  }
  /** The newest published comments across the site, with their items; a page may under-fill. */
  latest(q: PageQuery = {}, signal?: AbortSignal): Promise<FeedItem[]> {
    return call(this.http, "GET", "/comments/latest", { query: q, signal });
  }
  /** Every comment, newest first, deleted, held and rejected ones with their bodies (CommentModerate). */
  adminList(q: AdminCommentQuery = {}, signal?: AbortSignal): Promise<AdminComment[]> {
    const { contentKind, ...page } = q;
    return call(this.http, "GET", "/comments/admin", { query: { ...page, content_kind: contentKind }, signal });
  }
  /** Restores a deleted comment (CommentModerate). */
  async restore(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "POST", "/comments/{cid}/restore", { params: { cid: id } });
    emit({ type: "comment.restored", id });
  }
}

import type { CommentBan, CommentStanding, Comment, Content, Poll, ReactionCounts, ResolveInput } from "../generated/wire.js";

/** What an interaction targets: a content item (optionally one of its versions) or a group. */
export interface Target {
  type: "contents" | "groups";
  id: string;
  /** One version of a content item. */
  version?: string;
}

/** The same target. */
export function sameTarget(a?: Target, b?: Target): boolean {
  return !!a && !!b && a.type === b.type && a.id === b.id && (a.version ?? "") === (b.version ?? "");
}

/** A reaction: like 1, dislike -1, none 0. */
export type Reaction = -1 | 0 | 1;

/** The route verb that sets a like or dislike; clearing is DELETE …/reaction. */
export function reactionVerb(value: -1 | 1): "like" | "dislike" {
  return value === 1 ? "like" : "dislike";
}

/** Comment order: newest (the default), likes (most liked) or best (Wilson lower bound). */
export type Sort = "newest" | "likes" | "best";

/** A ban route family: "owner" bans from the caller's own content, "global" from the whole site. */
export type BanScope = CommentStanding["ban_scopes"][number];

/** What the review queue holds: comments and content items. */
export type HeldKind = "comment" | "content";

/** A reviewer's decision on a held item. */
export type Decision = Exclude<ResolveInput["decision"], "review">;

/** A content-module mutation made through the client. */
export type ContentChange =
  /** Created or edited: the item as the server now answers it. */
  | { type: "content.saved"; content: Content }
  /** A version changed: refetch the item. */
  | { type: "content.changed"; id: string }
  | { type: "content.deleted"; id: string }
  /** target is the commented item. */
  | { type: "comment.created"; target: Target; comment: Comment }
  | { type: "comment.updated"; comment: Comment }
  | { type: "comment.deleted"; id: string }
  | { type: "comment.restored"; id: string }
  | { type: "comment.reacted"; id: string; counts: ReactionCounts }
  | { type: "reaction.changed"; target: Target; counts: ReactionCounts }
  | { type: "favorite.changed"; target: Target; favorited: boolean; count: number }
  /** Created, updated, voted or answered: the poll as the server now answers it. */
  | { type: "poll.created"; poll: Poll }
  | { type: "poll.updated"; poll: Poll }
  /** An option or image changed: refetch the poll. */
  | { type: "poll.changed"; id: string }
  | { type: "poll.deleted"; id: string }
  | { type: "ban.saved"; scope: BanScope; ban: CommentBan }
  | { type: "ban.lifted"; scope: BanScope; user: string }
  | { type: "moderation.resolved"; kind: HeldKind; id: string; decision: Decision }
  | { type: "groups.changed" };

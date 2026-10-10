import type { CommentBan, CommentStanding, Comment, Poll, Post, ReactionCounts, RefBody, ResolveInput } from "../generated/wire.js";

/** A reaction: like 1, dislike -1, none 0. */
export type Reaction = -1 | 0 | 1;

/** The route verb that sets a reaction. */
export function reactionVerb(value: Reaction): "like" | "dislike" | "neutral" {
  return value === 1 ? "like" : value === -1 ? "dislike" : "neutral";
}

/** Comment and post order: newest (the default), likes (most liked) or best (Wilson lower bound). */
export type Sort = "newest" | "likes" | "best";

/** A ban route family: "owner" bans from the caller's own content, "global" from the whole site. */
export type BanScope = CommentStanding["ban_scopes"][number];

/** What the review queue holds. */
export type HeldKind = "comment" | "post";

/** A reviewer's decision on a held item. */
export type Decision = Exclude<ResolveInput["decision"], "review">;

/** A content-module mutation made through the client. */
export type ContentChange =
  | { type: "post.created"; post: Post }
  | { type: "post.updated"; post: Post }
  /** The cover changed: refetch the post. */
  | { type: "post.changed"; id: string }
  | { type: "post.deleted"; id: string }
  | { type: "post.restored"; post: Post }
  /** ref is the commented item. */
  | { type: "comment.created"; ref: RefBody; comment: Comment }
  | { type: "comment.updated"; comment: Comment }
  | { type: "comment.deleted"; id: string }
  | { type: "comment.restored"; id: string }
  | { type: "comment.reacted"; id: string; counts: ReactionCounts }
  | { type: "reaction.changed"; ref: RefBody; counts: ReactionCounts }
  | { type: "favorite.changed"; ref: RefBody; favorited: boolean }
  /** Created, updated, voted or answered: the poll as the server now answers it. */
  | { type: "poll.created"; poll: Poll }
  | { type: "poll.updated"; poll: Poll }
  /** An option or image changed: refetch the poll. */
  | { type: "poll.changed"; id: string }
  | { type: "poll.deleted"; id: string }
  | { type: "ban.saved"; scope: BanScope; ban: CommentBan }
  | { type: "ban.lifted"; scope: BanScope; user: string }
  | { type: "moderation.resolved"; kind: HeldKind; id: string; decision: Decision }
  | { type: "taxonomy.changed" };

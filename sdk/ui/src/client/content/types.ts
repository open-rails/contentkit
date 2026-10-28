import type { CommentBan, CommentStanding, Comment, Content, Poll, ReactionCounts, ResolveInput } from "../generated/wire.js";

/** What an interaction targets: a content item (optionally one of its versions), metadata entity or channel. */
export interface Target {
  type: "contents" | "entities" | "channels";
  id: string;
  /** One version of a content item. */
  version?: string;
}

/** A target that takes comments and favorites; entities take reactions only. */
export type CommentTarget = Target & { type: "contents" | "channels" };

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
export type HeldKind = "comment";

/** The moderation rules: banned words or IP bans. */
export type RuleSet = "words" | "ips";

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
  /** count is absent where the target's kind's locked view hides it. */
  | { type: "favorite.changed"; target: Target; favorited: boolean; count?: number }
  /** Created, updated, voted or answered: the poll as the server now answers it. */
  | { type: "poll.created"; poll: Poll }
  | { type: "poll.updated"; poll: Poll }
  /** An option or image changed: refetch the poll. */
  | { type: "poll.changed"; id: string }
  | { type: "poll.deleted"; id: string }
  | { type: "ban.saved"; scope: BanScope; ban: CommentBan }
  | { type: "ban.lifted"; scope: BanScope; user: string }
  | { type: "moderation.resolved"; kind: HeldKind; id: string; decision: Decision }
  /** Banned words or IP bans changed. */
  | { type: "rules.changed"; rules: RuleSet }
  /** A version's reader metadata changed: its reader plan reads again. */
  | { type: "reader.changed"; content: string; version: string }
  | { type: "entities.changed" }
  | { type: "channels.changed"; id?: string }
  | { type: "channel.reposts.changed"; id: string }
  /** The reader's content filters changed: every list reads again. */
  | { type: "contentFilters.changed" }
  | { type: "history.changed" }
  | { type: "engagement.changed"; ids: string[] }
  /** A search ran or recent searches were cleared. */
  | { type: "searches.changed" }
  /** The reader's keys changed (a join, a purchase the host reports): reads of access re-run. */
  | { type: "access.changed" };

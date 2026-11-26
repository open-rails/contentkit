import { useCallback, useEffect, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { FeedbackAnswerInput, FeedbackReaction, FeedbackTarget } from "../client/feedback.js";
import type { ReactionFeedback } from "../client/generated/wire.js";
import { useContentKitClient } from "./context.js";

export interface UseReactionFeedback {
  /** The prompt to show, answered or not; null when there is none or it was dismissed. */
  prompt: ReactionFeedback | null;
  pending: boolean;
  error?: ContentKitError;
  /** Asks after the reader's reaction; resolves with the prompt, or null. */
  ask: (reaction: FeedbackReaction) => Promise<ReactionFeedback | null>;
  answer: (answer: FeedbackAnswerInput) => Promise<void>;
  /** Takes or declines the prompt's hide suggestion. */
  hide: (take: boolean) => Promise<void>;
  dismiss: () => void;
}

/**
 * The feedback prompt for an item. With `follow` (the default) it asks by
 * itself after each like, favorite or dislike of the item made through the
 * client, such as by `ReactionButtons` and `FavoriteButton`.
 */
export function useReactionFeedback(item: FeedbackTarget, o: { client?: ContentKitClient; follow?: boolean } = {}): UseReactionFeedback {
  const client = useContentKitClient(o.client);
  const { id, version } = item;
  const [prompt, setPrompt] = useState<ReactionFeedback | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ContentKitError>();
  const current = useRef<ReactionFeedback | null>(null);
  current.current = prompt;

  const run = useCallback(async <T,>(fn: () => Promise<T>): Promise<T> => {
    setPending(true);
    setError(undefined);
    try {
      return await fn();
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      throw err;
    } finally {
      setPending(false);
    }
  }, []);

  const ask = useCallback(
    (reaction: FeedbackReaction) =>
      run(async () => {
        const next = await client.feedback.ask({ id, version }, reaction);
        if (next) setPrompt(next);
        return next;
      }),
    [client, id, version, run],
  );
  const answer = useCallback(
    (a: FeedbackAnswerInput) => run(async () => void setPrompt(await client.feedback.answer({ id, version }, a))),
    [client, id, version, run],
  );
  const hide = useCallback(
    (take: boolean) =>
      run(async () => {
        if (current.current) setPrompt(await client.feedback.hide({ id, version }, current.current, take));
      }),
    [client, id, version, run],
  );
  const dismiss = useCallback(() => setPrompt(null), []);

  const follow = o.follow ?? true;
  useEffect(() => {
    setPrompt(null);
    if (!follow) return;
    return client.subscribe((change) => {
      if (current.current) return;
      const mine = change.type === "reaction.changed" || change.type === "favorite.changed" ? change.target : undefined;
      if (!mine || mine.type !== "contents" || mine.id !== id || (mine.version ?? "") !== (version ?? "")) return;
      let reaction: FeedbackReaction | undefined;
      if (change.type === "favorite.changed" && change.favorited) reaction = "favorite";
      if (change.type === "reaction.changed" && change.counts.mine !== 0) reaction = change.counts.mine === 1 ? "like" : "dislike";
      if (reaction) void ask(reaction).catch(() => {});
    });
  }, [client, id, version, follow, ask]);

  return { prompt, pending, error, ask, answer, hide, dismiss };
}

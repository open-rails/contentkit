import type { DiscoveryClient } from "./discovery.js";
import { ContentKitError } from "./errors.js";
import type { FeedbackInput, FeedbackOffer, FeedbackRequest, ReactionFeedback } from "./generated/wire.js";
import type { Http } from "./http.js";
import type { ReaderPreferencesClient } from "./reader-preferences.js";
import { call } from "./route.js";

/** The reaction a prompt follows. */
export type FeedbackReaction = FeedbackRequest["reaction"];
/** An answer to a prompt. */
export type FeedbackReason = NonNullable<FeedbackInput["reason"]>;

/** The item a reaction was on: a content item, or one of its versions. */
export interface FeedbackTarget {
  id: string;
  version?: string;
}

/** An answer: one of the prompt's reasons, the entity it names, and text with "other". */
export interface FeedbackAnswerInput {
  reason: FeedbackReason;
  /** The tag (required for "tag" and "not_into"), artist or series; with one artist or series it may be left out. */
  entity_id?: string;
  /** With "other", at most 280 characters. */
  text?: string;
  /** With "not_into" a hideable tag: hide it (true) or decline (false). */
  hide?: boolean;
}

/**
 * Why a reader liked, favorited or disliked an item. Ask after a reaction;
 * the server samples, keeps about one prompt a day per person and never asks
 * twice about one item. The host or `ReactionFeedbackPrompt` renders it.
 */
export class FeedbackClient {
  constructor(
    private readonly http: Http,
    private readonly discovery: DiscoveryClient,
    private readonly preferences: ReaderPreferencesClient,
  ) {}

  /** The prompt for the reader's reaction to the item, recorded as offered; null when there is none. */
  async ask(item: FeedbackTarget, reaction: FeedbackReaction, signal?: AbortSignal): Promise<ReactionFeedback | null> {
    const out: FeedbackOffer = await call(this.http, "POST", "/contents/{id}/feedback", {
      params: { id: item.id },
      query: { version: item.version, language: this.http.language() },
      body: { reaction },
      signal,
    });
    return out.prompt;
  }

  /** Answers the item's prompt, a later answer replacing it; see `hide` for answer.hide. */
  answer(item: FeedbackTarget, answer: FeedbackAnswerInput): Promise<ReactionFeedback> {
    const { reason, entity_id, text, hide } = answer;
    return this.decide(item, { reason, entity_id, text, hide }, hide && entity_id);
  }

  /**
   * Takes or declines hiding the tag a "not_into" answer named, once the
   * prompt suggests it (prompt.hide). Taken while signed in, the account's
   * content filters hide the tag; signed out, this browser's do, and they
   * carry over at sign-in.
   */
  hide(item: FeedbackTarget, prompt: ReactionFeedback, take: boolean): Promise<ReactionFeedback> {
    if (!prompt.hide) throw new ContentKitError("invalid_request", "The prompt suggests no tag to hide.");
    return this.decide(item, { hide: take }, take && prompt.hide.id);
  }

  private async decide(item: FeedbackTarget, body: FeedbackInput, hidden?: string | false): Promise<ReactionFeedback> {
    const emit = this.http.captureChanges();
    const out = await this.patch(item, body);
    if (hidden && this.http.signedIn()) emit({ type: "contentFilters.changed" });
    else if (hidden) {
      await this.discovery.contentFilters();
      this.preferences.setGuestFilters([{ entity_id: hidden, visible: false }]);
    }
    return out;
  }

  private patch(item: FeedbackTarget, body: FeedbackInput): Promise<ReactionFeedback> {
    return call(this.http, "PATCH", "/contents/{id}/feedback", {
      params: { id: item.id },
      query: { language: this.http.language() },
      body,
    });
  }
}

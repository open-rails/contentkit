import { createContext, useCallback, useContext, useRef } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { ContentURLs } from "../urls/index.js";
import type { ReadStore } from "./store.js";

/** What failed: a component's or hook's save, load or render step. */
export type ContentKitOperation =
  | "poster.load"
  | "poster.frame"
  | "poster.save"
  | "slot.load"
  | "slot.decode"
  | "slot.save"
  | "slot.remove"
  | "upload"
  | SocialOperation;

/** What failed in a content component: a load or a write. */
export type SocialOperation =
  | "comment.load"
  | "comment.post"
  | "comment.edit"
  | "comment.delete"
  | "comment.react"
  | "comment.restore"
  | "ban.save"
  | "ban.lift"
  | "reaction.save"
  | "favorite.save"
  | "poll.load"
  | "poll.vote"
  | "poll.answer"
  | "poll.save"
  | "post.load"
  | "post.save"
  | "moderation.load"
  | "moderation.resolve";

/**
 * Every failure a component shows is also reported here (aborts excepted), so
 * the host can toast or log it. Components still show it in place.
 */
export type ContentKitErrorHandler = (error: ContentKitError, info: { operation: ContentKitOperation }) => void;

/** The host router: in-app navigation, e.g. a canonical replace. */
export type Navigate = (to: string, options?: { replace?: boolean }) => void;

export interface ContentKitContextValue {
  client: ContentKitClient;
  /**
   * The signed-in user's id as ContentKit sees it, null when signed out,
   * undefined when the host does not say. Content reads are kept per viewer.
   */
  viewer?: string | null;
  /** Asks the visitor to sign in; with it, signed-out visitors are prompted instead of acting anonymously. */
  onSignIn?: () => void;
  /** The host's content URL config (createContentURLs). */
  urls?: ContentURLs;
  navigate?: Navigate;
  onError?: ContentKitErrorHandler;
  /** The reads hooks share. */
  store: ReadStore;
}

export const ContentKitContext = createContext<ContentKitContextValue | null>(null);

/** The ContentKitProvider's value; throws outside one. */
export function useContentKit(): ContentKitContextValue {
  const ctx = useContext(ContentKitContext);
  if (!ctx) throw new Error("contentkit-ui: wrap the tree in <ContentKitProvider client>");
  return ctx;
}

/** The client a hook or component uses: its own, else the provider's; throws without either. */
export function useContentKitClient(own?: ContentKitClient | null): ContentKitClient {
  const c = useOptionalContentKitClient(own);
  if (!c) throw new Error("contentkit-ui: pass `client` or render inside <ContentKitProvider client>");
  return c;
}

export function useOptionalContentKitClient(own?: ContentKitClient | null): ContentKitClient | null {
  const ctx = useContext(ContentKitContext);
  return own ?? ctx?.client ?? null;
}

/** The provider's content URL helpers; throws when none were given. */
export function useContentURLs(): ContentURLs {
  const urls = useContext(ContentKitContext)?.urls;
  if (!urls) throw new Error("contentkit-ui: pass `urls` to <ContentKitProvider>");
  return urls;
}

/** A stable reporter: the component's own `onError`, else the provider's. */
export function useErrorReporter(own?: ContentKitErrorHandler): (e: unknown, operation: ContentKitOperation) => void {
  const ctx = useContext(ContentKitContext)?.onError;
  const handler = useRef(own ?? ctx);
  handler.current = own ?? ctx;
  return useCallback((e: unknown, operation: ContentKitOperation) => {
    const error = toContentKitError(e);
    if (error.code !== "aborted") handler.current?.(error, { operation });
  }, []);
}

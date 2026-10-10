import { FavouriteIcon, ThumbsDownIcon, ThumbsUpIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import { toContentKitError } from "../../client/errors.js";
import type { ReactionCounts, RefBody } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useFavorite, useReaction } from "../../react/engagement.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { contentError } from "./parts.js";

/** Signed out with a sign-in to offer: the action asks for it instead of running. */
function useSignInGate(own?: () => void) {
  const ctx = useContext(ContentKitContext);
  const signIn = own ?? ctx?.onSignIn;
  return (fn: () => void) => (ctx?.viewer === null && signIn ? signIn() : fn());
}

const compact = (n: number, language?: string) => new Intl.NumberFormat(language, { notation: "compact", maximumFractionDigits: 1 }).format(n);

export interface ReactionButtonsProps {
  item: RefBody;
  /** Counts the host already has, used instead of reading them. */
  counts?: ReactionCounts;
  /** Offer dislike too. Default true. */
  dislike?: boolean;
  /** Asks a signed-out visitor to sign in (default the provider's); without one they react anonymously. */
  onSignIn?: () => void;
  size?: "sm" | "default";
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Like and dislike toggles with counts: they change at once and roll back if the server refuses. */
export function ReactionButtons({ item, counts: initial, dislike = true, onSignIn, size = "default", client, onError, className, appearance }: ReactionButtonsProps) {
  const m = useMessages();
  const { t } = m;
  const r = useReaction(item, { initial, client });
  const gate = useSignInGate(onSignIn);
  const report = useErrorReporter(onError);
  const [error, setError] = useState<ContentKitError | null>(null);
  const toggle = (v: 1 | -1) =>
    gate(() => {
      setError(null);
      r.toggle(v).catch((e) => {
        const err = toContentKitError(e);
        setError(err);
        report(err, "reaction.save");
      });
    });
  const btn = size === "sm" ? "sm" : "default";
  const ctx = useContext(ContentKitContext);
  const hint = ctx?.viewer === null && (onSignIn ?? ctx?.onSignIn) ? t("reactions.signIn") : undefined;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("inline-flex flex-wrap items-center gap-1 text-sm", className)} data-ckui="reaction-buttons">
      <div className="inline-flex items-center gap-1">
        <Button variant={r.counts.mine === 1 ? "secondary" : "ghost"} size={btn} aria-pressed={r.counts.mine === 1} aria-label={t("reactions.like")} title={hint} onClick={() => toggle(1)}>
          <HugeiconsIcon icon={ThumbsUpIcon} strokeWidth={2} />
          <span>{compact(r.counts.likes, m.language)}</span>
        </Button>
        {dislike && (
          <Button variant={r.counts.mine === -1 ? "secondary" : "ghost"} size={btn} aria-pressed={r.counts.mine === -1} aria-label={t("reactions.dislike")} title={hint} onClick={() => toggle(-1)}>
            <HugeiconsIcon icon={ThumbsDownIcon} strokeWidth={2} />
            <span>{compact(r.counts.dislikes, m.language)}</span>
          </Button>
        )}
      </div>
      {error && (
        <span role="alert" className="text-xs text-destructive">
          {error.code === "rate_limited" || error.code === "forbidden" ? contentError(m, error) : t("reactions.failed")}
        </span>
      )}
    </ContentKitUiRoot>
  );
}

export interface FavoriteButtonProps {
  item: RefBody;
  /** Whether it is favorited, as the host already knows; used instead of reading it. */
  favorited?: boolean;
  /** The item's favorite count, as the host has it; moved by one with each change. */
  count?: number;
  /** Show the label beside the icon. Default true. */
  label?: boolean;
  /** Asks a signed-out visitor to sign in (default the provider's). */
  onSignIn?: () => void;
  /** After each saved change. */
  onChange?: (favorited: boolean) => void;
  size?: "sm" | "default";
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Adds the item to the signed-in visitor's favorites, or removes it; changes at once and rolls back if refused. */
export function FavoriteButton({ item, favorited: initial, count, label = true, onSignIn, onChange, size = "default", client, onError, className, appearance }: FavoriteButtonProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const f = useFavorite(item, { initial, client });
  const signIn = onSignIn ?? ctx?.onSignIn;
  const report = useErrorReporter(onError);
  const [error, setError] = useState<ContentKitError | null>(null);
  const [status, setStatus] = useState("");
  // The count the host gave counts the state it knew; move it with ours.
  const shown = count === undefined ? undefined : count + (f.favorited ? 1 : 0) - (initial ? 1 : 0);
  const click = () => {
    if (ctx?.viewer === null) return signIn?.();
    setError(null);
    const next = !f.favorited;
    f.set(next).then(
      () => {
        setStatus(t(next ? "favorites.added" : "favorites.removed"));
        onChange?.(next);
      },
      (e) => {
        const err = toContentKitError(e);
        if (err.code === "unauthorized" && signIn) return signIn();
        setError(err);
        report(err, "favorite.save");
      },
    );
  };
  const text = t(ctx?.viewer === null && signIn ? "favorites.signIn" : f.favorited ? "favorites.remove" : "favorites.add");
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("inline-flex flex-wrap items-center gap-2 text-sm", className)} data-ckui="favorite-button">
      <Button
        variant={f.favorited ? "secondary" : "outline"}
        size={label ? (size === "sm" ? "sm" : "default") : size === "sm" ? "icon-sm" : "icon"}
        aria-pressed={f.favorited}
        aria-label={label ? undefined : text}
        disabled={f.pending}
        onClick={click}
      >
        <HugeiconsIcon icon={FavouriteIcon} strokeWidth={2} className={cn(f.favorited && "fill-current text-destructive")} />
        {label && <span>{text}</span>}
        {shown !== undefined && <span className="tabular-nums text-muted-foreground">{compact(Math.max(0, shown), m.language)}</span>}
      </Button>
      <span className="sr-only" role="status">
        {status}
      </span>
      {error && (
        <span role="alert" className="text-xs text-destructive">
          {contentError(m, error)}
        </span>
      )}
    </ContentKitUiRoot>
  );
}

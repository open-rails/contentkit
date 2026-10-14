import { FavouriteIcon, LockIcon, ThumbsUpIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useContext, type MouseEvent, type ReactNode } from "react";
import type { Card, CardAccess } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { ContentKitContext } from "../../react/context.js";
import { Badge } from "#ckui/ui/badge";
import { RenditionImg } from "../rendition-img.js";
import { formatDate, RelativeTime } from "../social/parts.js";
import { GroupAvatar, GroupLink, labelOf, type GroupLike, type Labels } from "../groups/parts.js";
import { priceLabel } from "./paywall.js";

export interface ContentCardProps {
  card: Card;
  /** The card's link; default its canonical path. */
  href?: (card: Card) => string | undefined;
  /** Before the card's link is followed: a click signal. */
  onOpen?: (card: Card) => void;
  /** Cover shape: "portrait" (3:4, readers) or "landscape" (16:9, players). Default portrait. */
  shape?: "portrait" | "landscape";
  /** The group kinds whose chips show, in order; default every kind the card names. */
  groupKinds?: readonly string[];
  /** Chips per group kind. Default 3. */
  chips?: number;
  /** Tiers the site sells, so a lock names them; others show as "for sale". */
  tiers?: readonly string[];
  /** Display names of kinds, traits and tiers by identifier. */
  labels?: Labels;
  /** Show the owner group (a channel) when the card has one; default true, false on that group's own feed. */
  owner?: boolean;
  /** The owner group's link; default its canonical path. */
  ownerHref?: (g: GroupLike) => string | undefined;
  /** Over the cover: the host's badges for the card, placed in the cover's box; clicks reach the card. */
  decorate?: (card: Card) => ReactNode;
  /** Below the meta line: the host's own. */
  children?: ReactNode;
  className?: string;
}

/** The host's badges over a cover. */
function Decorations({ card, decorate }: { card: Card; decorate?: (card: Card) => ReactNode }) {
  const badges = decorate?.(card);
  if (badges == null || badges === false) return null;
  return (
    <span className="pointer-events-none absolute inset-0" data-ckui="card-decorations">
      {badges}
    </span>
  );
}

const compact = (n: number, language?: string) => new Intl.NumberFormat(language, { notation: "compact", maximumFractionDigits: 1 }).format(n);

/** Why a card is locked, in words (its price when it is on sale); null when it opens. */
export function lockLabel(m: Translator, a: CardAccess, tiers: readonly string[] = [], labels?: Labels): string | null {
  if (a.allowed) return null;
  if (a.unknown) return m.t("contents.unknownAccess");
  const offer = a.offers?.[0];
  if (offer) return offer.target.kind === "group" && Number(offer.terms.amount) === 0 ? m.t("contents.joinFree") : priceLabel(m, offer.terms);
  const tier = a.sell?.find((k) => tiers.includes(k));
  if (tier) return labelOf(labels, tier);
  if (a.sell?.some((k) => k.includes(":group:"))) return m.t("contents.members");
  if (a.sell?.length) return m.t("contents.forSale");
  return m.t("contents.locked");
}

/** A link followed in-app through the provider's navigate. */
function useLink(url: string | undefined, before?: () => void) {
  const navigate = useContext(ContentKitContext)?.navigate;
  return useCallback(
    (e: MouseEvent<HTMLAnchorElement>) => {
      before?.();
      if (!navigate || !url || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      navigate(url);
    },
    [navigate, url, before],
  );
}

/** One content in a listing: cover, title, version, groups, counts, the reader's progress and why it is locked; what its kind's locked view hides is absent. */
export function ContentCard({ card, href, onOpen, shape = "portrait", groupKinds, chips = 3, tiers, labels, owner = true, ownerHref, decorate, children, className }: ContentCardProps) {
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const url = href ? href(card) : card.path;
  const open = useCallback(() => onOpen?.(card), [onOpen, card]);
  const onClick = useLink(url, open);
  const lock = lockLabel(m, card.access, tiers, labels);
  const v = card.version;
  const progress = card.viewer && card.viewer.progress_max > 0 ? Math.min(1, card.viewer.progress / card.viewer.progress_max) : 0;
  const kinds = groupKinds ?? Object.keys(card.groups);
  const minutes = v.duration_ms ? Math.round(v.duration_ms / 60_000) : 0;
  const meta = [
    ...v.languages.map(languageName),
    ...v.traits.map((x) => labelOf(labels, x)),
    v.pages ? m.plural("contents.pages", v.pages) : null,
    minutes ? m.plural("contents.minutes", minutes) : null,
  ].filter(Boolean);
  const title = card.title || card.code;
  return (
    <article className={cn("group/card grid content-start gap-2", className)} data-ckui="content-card" data-id={card.id} data-locked={lock ? "" : undefined} lang={card.language || undefined}>
      <div className="relative overflow-hidden rounded-lg">
        <a href={url} onClick={onClick} className="relative block overflow-hidden rounded-lg bg-muted" tabIndex={-1} aria-hidden>
          <div className={shape === "landscape" ? "aspect-video" : "aspect-[3/4]"}>
            <RenditionImg
              outputs={card.cover}
              alt=""
              fit="cover"
              skeleton
              className="size-full transition-transform group-hover/card:scale-[1.02]"
              fallback={<span className="grid size-full place-items-center text-2xl font-semibold text-muted-foreground">{title.charAt(0).toUpperCase()}</span>}
            />
          </div>
          {lock && (
            <span className="absolute top-2 left-2 inline-flex items-center gap-1 rounded-md bg-background/85 px-1.5 py-0.5 text-xs font-medium text-foreground shadow-sm" data-ckui="card-lock">
              <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-hidden />
              {lock}
            </span>
          )}
          {progress > 0 && (
            <span className="absolute inset-x-0 bottom-0 h-1 bg-background/60" data-ckui="card-progress">
              <span className="block h-full bg-primary" style={{ width: `${Math.round(progress * 100)}%` }} />
            </span>
          )}
        </a>
        <Decorations card={card} decorate={decorate} />
      </div>
      <div className="grid gap-1">
        <a href={url} onClick={onClick} className="line-clamp-2 font-medium break-words hover:underline" data-ckui="card-title">
          {title}
          {lock && <span className="sr-only"> ({lock})</span>}
        </a>
        {owner && card.owner && <OwnerChip group={card.owner} href={ownerHref} />}
        {meta.length > 0 && <p className="text-xs text-muted-foreground">{meta.join(" · ")}</p>}
        {card.viewer?.completed && (
          <Badge variant="secondary" className="justify-self-start">
            {t("contents.completed")}
          </Badge>
        )}
        {!card.access.allowed && card.access.free_after && <p className="text-xs text-muted-foreground">{t("contents.freeFrom", { date: formatDate(m, card.access.free_after, false) })}</p>}
        {kinds.some((k) => card.groups[k]?.length) && (
          <ul className="flex flex-wrap gap-1" data-ckui="card-groups">
            {kinds.flatMap((k) =>
              (card.groups[k] ?? []).slice(0, chips).map((g) => (
                <li key={`${k}:${g.id}`} data-kind={k}>
                  <Chip url={g.path} name={g.name || g.slug} />
                </li>
              )),
            )}
          </ul>
        )}
        {(card.counts.favorites !== undefined || card.counts.likes !== undefined) && (
          <p className="flex items-center gap-3 text-xs text-muted-foreground tabular-nums" data-ckui="card-counts">
            {card.counts.favorites !== undefined && (
              <span className="inline-flex items-center gap-1" title={t("contents.favorites")}>
                <HugeiconsIcon icon={FavouriteIcon} strokeWidth={2} className={cn("size-3.5", card.viewer?.favorited && "fill-current text-primary")} aria-hidden />
                <span className="sr-only">{t("contents.favorites")}</span>
                {compact(card.counts.favorites, m.language)}
              </span>
            )}
            {card.counts.likes !== undefined && (
              <span className="inline-flex items-center gap-1" title={t("contents.likes")}>
                <HugeiconsIcon icon={ThumbsUpIcon} strokeWidth={2} className="size-3.5" aria-hidden />
                <span className="sr-only">{t("contents.likes")}</span>
                {compact(card.counts.likes, m.language)}
              </span>
            )}
          </p>
        )}
        {children}
      </div>
    </article>
  );
}

/** The group that owns a content (a channel): its avatar and name, linking to its page. */
export function OwnerChip({ group, href, className }: { group: GroupLike; href?: (g: GroupLike) => string | undefined; className?: string }) {
  return (
    <span className={cn("flex min-w-0 text-xs text-muted-foreground", className)} data-ckui="card-owner" data-id={group.id}>
      <GroupLink group={group} href={href} className="inline-flex min-w-0 items-center gap-1.5 hover:text-foreground">
        <GroupAvatar group={group} className="size-5 shrink-0 text-[0.625rem]" />
        <span className="truncate">{group.name || group.slug}</span>
      </GroupLink>
    </span>
  );
}

function Chip({ url, name }: { url?: string; name: string }) {
  const onClick = useLink(url);
  const cls = "inline-block rounded-md border border-border px-1.5 py-0.5 text-xs text-muted-foreground";
  return url ? (
    <a href={url} onClick={onClick} className={cn(cls, "hover:bg-muted hover:text-foreground")}>
      {name}
    </a>
  ) : (
    <span className={cls}>{name}</span>
  );
}

export interface PostCardProps extends Omit<ContentCardProps, "shape" | "chips" | "groupKinds"> {
  /** Above the title: the post's channel or author; default its owner group's chip. */
  byline?: ReactNode;
}

/** A post in a feed: a wide cover, its title, when it was published and what unlocking it costs. */
export function PostCard({ card, href, onOpen, tiers, labels, byline, owner = true, ownerHref, decorate, children, className }: PostCardProps) {
  const m = useMessages();
  const url = href ? href(card) : card.path;
  const open = useCallback(() => onOpen?.(card), [onOpen, card]);
  const onClick = useLink(url, open);
  const lock = lockLabel(m, card.access, tiers, labels);
  const title = card.title || card.code;
  return (
    <article className={cn("grid gap-3 rounded-lg border border-border p-3", className)} data-ckui="post-card" data-id={card.id} data-locked={lock ? "" : undefined} lang={card.language || undefined}>
      {byline ?? (owner && card.owner && <OwnerChip group={card.owner} href={ownerHref} className="text-sm" />)}
      <a href={url} onClick={onClick} className="grid gap-2">
        <span className="font-medium break-words hover:underline" data-ckui="card-title">
          {title}
        </span>
        {card.cover.length > 0 && (
          <span className="relative block aspect-video overflow-hidden rounded-md bg-muted">
            <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton className={cn("size-full", lock && "blur-md")} />
            <Decorations card={card} decorate={decorate} />
            {lock && (
              <span className="absolute inset-0 grid place-items-center">
                <span className="inline-flex items-center gap-1 rounded-md bg-background/85 px-2 py-1 text-xs font-medium shadow-sm" data-ckui="card-lock">
                  <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-hidden />
                  {lock}
                </span>
              </span>
            )}
          </span>
        )}
      </a>
      <p className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
        {card.published_at && <RelativeTime at={card.published_at} />}
        {!card.cover.length && lock && <span data-ckui="card-lock">{lock}</span>}
        {card.counts.favorites !== undefined && (
          <span className="inline-flex items-center gap-1 tabular-nums" data-ckui="card-counts">
            <HugeiconsIcon icon={FavouriteIcon} strokeWidth={2} className={cn("size-3.5", card.viewer?.favorited && "fill-current text-primary")} aria-hidden />
            <span className="sr-only">{m.t("contents.favorites")}</span>
            {compact(card.counts.favorites, m.language)}
          </span>
        )}
      </p>
      {children}
    </article>
  );
}

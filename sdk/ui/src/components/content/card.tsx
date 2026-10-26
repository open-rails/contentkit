import { FavouriteIcon, LockIcon, SparklesIcon, ThumbsUpIcon, Tick02Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { ReleaseNotice } from "./release-notice.js";
import { cn } from "cn";
import { ChannelAvatar, ChannelLink, type ChannelLike } from "../channels/parts.js";
import { Fragment, useCallback, useContext, useLayoutEffect, useState, type CSSProperties, type MouseEvent, type ReactNode } from "react";
import type { Card, CardAccess, Chip as EntityChip } from "../../client/generated/wire.js";
import type { ContentKitClient } from "../../client/client.js";
import { ratio } from "../../client/aspect.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { ContentKitContext } from "../../react/context.js";
import { usePresets } from "../../react/public.js";
import { Badge } from "#ckui/ui/badge";
import { HoverPreview } from "../hover-preview.js";
import { RenditionImg } from "../rendition-img.js";
import { RelativeTime } from "../social/parts.js";
import { labelOf, type Labels } from "../entities/parts.js";

export interface ContentCardProps {
  card: Card;
  /** Overrides the provider's client for this card's public media configuration. */
  client?: ContentKitClient;
  /** The card's link; default its canonical path. */
  href?: (card: Card) => string | undefined;
  /** Before the card's link is followed: a click signal. */
  onOpen?: (card: Card) => void;
  /** Fallback cover shape when the server declares no preset aspect. Default portrait (3:4). */
  shape?: "portrait" | "landscape";
  /** A CSS aspect ratio overriding shape, such as "182 / 260". */
  aspectRatio?: CSSProperties["aspectRatio"];
  /** Place the title below the cover (default), or over its bottom gradient. */
  titlePosition?: "below" | "cover";
  /** Mount images and previews. False keeps the card's text and reserved cover space. */
  loadMedia?: boolean;
  /** The group kinds whose chips show, in order; default every kind the card names. */
  entityKinds?: readonly string[];
  /** Chips per group kind, or `{ base, lg }` to show a different number from the lg breakpoint. Default 3. */
  chips?: number | { base: number; lg: number };
  /** After a kind's chips, a "+N" chip counting the ones left out. Default false. */
  chipOverflow?: boolean;
  /** Inline chips on one line, as many whole chips as fit (at least one); the "+N" counts the rest. Default false. */
  chipFit?: boolean;
  /** Select which entity chips appear, before the per-kind chip limit. */
  filterEntity?: (entity: EntityChip, kind: string) => boolean;
  /** Bordered chips (default) or plain linked text. */
  entityStyle?: "chips" | "text";
  /** One inline list (default), or a separate row for each group kind. */
  entityLayout?: "inline" | "rows";
  /** Metadata fields to show; omitted fields keep their defaults. */
  metadata?: CardMetadata;
  /** Show like and favorite totals. Default true. */
  showCounts?: boolean;
  /** Tiers the site sells, so a lock names them; others show as "for sale". */
  tiers?: readonly string[];
  /** Display names of kinds, traits and tiers by identifier. */
  labels?: Labels;
  /** Show the original channel; defaults true, including reposted content. */
  showChannel?: boolean;
  /** The original channel's link; default its canonical path. */
  channelHref?: (g: ChannelLike) => string | undefined;
  /** Over the cover: the host's badges for the card, placed in the cover's box; clicks reach the card. */
  decorate?: (card: Card) => ReactNode;
  /** The title the card shows, e.g. without a prefix its entity rows repeat; default its title, else its code. */
  cardTitle?: (card: Card) => string;
  /** Mark updated, completed and (for staff) deleted cards over the cover. Default true; false when the host draws cardStatus itself. */
  status?: boolean;
  /** Dim the cover of what the reader disliked. Default true. */
  dimDisliked?: boolean;
  /** Below the meta line: the host's own. */
  children?: ReactNode;
  className?: string;
}

export interface CardMetadata {
  languages?: boolean;
  /** True shows all traits; an array selects specific trait identifiers. */
  traits?: boolean | readonly string[];
  pages?: boolean;
  duration?: boolean;
  /** Entity kinds named last in the meta line: the first entity of the first kind the card has, linked. */
  entities?: readonly string[];
}

/** What a card says about the reader's standing and the work's state. */
export interface CardStatus {
  /** A newer version since the reader last read it, else within the last two weeks. */
  updated: boolean;
  completed: boolean;
  /** Only staff and the item's creators are told. */
  deleted: boolean;
  disliked: boolean;
}

const RECENT_MS = 14 * 24 * 60 * 60 * 1000;

export function cardStatus(card: Card, now = Date.now()): CardStatus {
  const updated = card.updated_at ? Date.parse(card.updated_at) : NaN;
  const read = card.viewer?.last_read_at ? Date.parse(card.viewer.last_read_at) : NaN;
  return {
    updated: Number.isFinite(updated) && (Number.isFinite(read) ? updated > read : now - updated < RECENT_MS),
    completed: !!card.viewer?.completed,
    deleted: !!card.deleted,
    disliked: card.viewer?.reaction === -1,
  };
}

/** Quiet marks for a card's status: updated, completed and deleted. */
export function CardStatusBadges({ card, className }: { card: Card; className?: string }) {
  const { t } = useMessages();
  const st = cardStatus(card);
  if (!st.updated && !st.completed && !st.deleted) return null;
  const icon = "size-4 text-white drop-shadow-[0_0_3px_rgba(0,0,0,0.9)]";
  return (
    <span className={cn("pointer-events-none flex items-center gap-1.5", className)} data-ckui="card-status">
      {st.deleted && <span className="rounded-md bg-background/85 px-1.5 py-0.5 text-xs font-medium text-foreground shadow-sm">{t("contents.deleted")}</span>}
      {st.updated && <HugeiconsIcon icon={SparklesIcon} strokeWidth={2} className={icon} role="img" aria-label={t("contents.updated")} />}
      {st.completed && <HugeiconsIcon icon={Tick02Icon} strokeWidth={2.5} className={icon} role="img" aria-label={t("contents.completed")} />}
    </span>
  );
}

/** The host's badges over a cover. */
function Decorations({ card, decorate, aboveTitle }: { card: Card; decorate?: (card: Card) => ReactNode; aboveTitle?: boolean }) {
  const badges = decorate?.(card);
  if (badges == null || badges === false) return null;
  return (
    <span className={cn("pointer-events-none absolute inset-0", aboveTitle && "z-20")} data-ckui="card-decorations">
      {badges}
    </span>
  );
}

const compact = (n: number, language?: string) => new Intl.NumberFormat(language, { notation: "compact", maximumFractionDigits: 1 }).format(n);

/** Why a card is locked, without copying prices from the billing catalog. */
export function lockLabel(m: Translator, a: CardAccess, tiers: readonly string[] = [], labels?: Labels): string | null {
  if (a.allowed) return null;
  if (a.unknown) return m.t("contents.unknownAccess");
  const tier = a.sell?.find((k) => tiers.includes(k));
  if (tier) return labelOf(labels, tier);
  if (a.sell?.some((k) => k.includes(":group:"))) return m.t("contents.members");
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

/** Which chips of a one-line list fit whole beside its "+N": `through` counts chips in list order (breakpoint-hidden ones included), `fitted` the ones that show, `squeezed` when even the first one does not fit and must shrink; null until measured. `chipsKey` names the chips rendered, so new ones are measured. */
function useChipFit(list: HTMLUListElement | null, total: number, chipsKey: string) {
  const [fit, setFit] = useState<{ through: number; fitted: number; squeezed: boolean } | null>(null);
  useLayoutEffect(() => {
    if (!list) return;
    const view = list.ownerDocument.defaultView;
    const measure = () => {
      const gap = Number.parseFloat(view?.getComputedStyle(list).columnGap ?? "") || 0;
      const items = [...list.children] as HTMLElement[];
      const more = items.find((li) => li.dataset.ckui === "card-chip-more")?.offsetWidth ?? 0;
      let used = 0;
      let through = 0;
      let fitted = 0;
      let first = -1;
      const chips = items.filter((li) => li.dataset.ckui !== "card-chip-more");
      for (const [i, chip] of chips.entries()) {
        if (view?.getComputedStyle(chip).display === "none") continue;
        if (first < 0) first = i;
        const next = used + (fitted > 0 ? gap : 0) + chip.offsetWidth;
        if (next + (total > fitted + 1 && more > 0 ? gap + more : 0) > list.clientWidth) break;
        used = next;
        through = i + 1;
        fitted += 1;
      }
      const squeezed = fitted === 0 && first >= 0;
      if (squeezed) [through, fitted] = [first + 1, 1];
      setFit((f) => f?.through === through && f.fitted === fitted && f.squeezed === squeezed ? f : { through, fitted, squeezed });
    };
    measure();
    // Each chip too: a breakpoint hiding one, or a web font loading, changes a chip but not the list.
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    observer?.observe(list);
    for (const li of list.children) observer?.observe(li);
    return () => observer?.disconnect();
  }, [list, total, chipsKey]);
  return fit;
}

/** One content in a listing: cover (a video the reader opens previews over it on hover), title, version, groups, counts, the reader's progress and why it is locked; what its kind's locked view hides is absent. */
export function ContentCard({ card, client, href, onOpen, shape = "portrait", aspectRatio, titlePosition = "below", loadMedia = true, entityKinds, chips = 3, chipOverflow = false, chipFit = false, filterEntity, entityStyle = "chips", entityLayout = "inline", metadata, showCounts = true, tiers, labels, showChannel = true, channelHref, decorate, cardTitle, status = true, dimDisliked = true, children, className }: ContentCardProps) {
  const m = useMessages();
  const { presets } = usePresets({ client });
  const coverAspect = aspectRatio ?? ratio(presets.find((p) => p.kind === card.kind && p.name === "cover")?.aspect);
  const { t } = m;
  const languageName = useLanguageName();
  const url = href ? href(card) : card.path;
  const open = useCallback(() => onOpen?.(card), [onOpen, card]);
  const onClick = useLink(url, open);
  const lock = lockLabel(m, card.access, tiers, labels);
  const st = cardStatus(card);
  const dim = dimDisliked && st.disliked;
  const v = card.version;
  const progress = card.viewer?.completed ? 1 : card.viewer && card.viewer.progress_max > 0 ? Math.min(1, card.viewer.progress / card.viewer.progress_max) : 0;
  const kinds = entityKinds ?? Object.keys(card.entities);
  const limit = typeof chips === "number" ? { base: chips, lg: chips } : chips;
  const entityGroups = kinds.map((kind) => {
    const all = (card.entities[kind] ?? []).filter((entity) => !filterEntity || filterEntity(entity, kind));
    return { kind, entities: all.slice(0, Math.max(limit.base, limit.lg)), total: all.length };
  }).filter(({ entities }) => entities.length > 0);
  const minutes = v.duration_ms ? Math.round(v.duration_ms / 60_000) : 0;
  const selectedTraits = metadata?.traits;
  const traits = selectedTraits === false ? [] : Array.isArray(selectedTraits) ? v.traits.filter((trait) => selectedTraits.includes(trait)) : v.traits;
  const metaEntity = metadata?.entities?.map((kind) => (card.entities[kind] ?? []).find((entity) => !filterEntity || filterEntity(entity, kind))).find(Boolean);
  const meta: ReactNode[] = [
    ...(metadata?.languages === false ? [] : v.languages.map(languageName)),
    metadata?.pages !== false && v.pages ? m.plural("contents.pages", v.pages) : null,
    metadata?.duration !== false && minutes ? m.plural("contents.minutes", minutes) : null,
    metaEntity ? <MetaLink key="group" url={metaEntity.path} name={metaEntity.name || metaEntity.slug} /> : null,
  ].filter(Boolean);
  const title = cardTitle?.(card) || card.title || card.code;
  const entityClassName = cn("flex flex-wrap", entityStyle === "text" ? "gap-x-[0.3em] gap-y-1 [&>li:not(:last-child)]:after:text-muted-foreground [&>li:not(:last-child)]:after:content-[',']" : "gap-1");
  // A chip past one breakpoint's limit shows only at the other.
  const atWidth = (i: number) => i < limit.base && i < limit.lg ? undefined : i < limit.base ? "lg:hidden" : "hidden lg:list-item";
  const overflowItems = (kind: string, total: number) => {
    if (!chipOverflow) return [];
    const base = total - Math.min(total, limit.base);
    const lg = total - Math.min(total, limit.lg);
    const more = (n: number, className?: string) => <li key={`${kind}:more:${className ?? ""}`} data-kind={kind} data-ckui="card-chip-more" className={className}>
      <span aria-hidden><Chip name={`+${n}`} variant={entityStyle} /></span>
      <span className="sr-only">{m.plural("contents.moreChips", n)}</span>
    </li>;
    if (base === lg) return base > 0 ? [more(base)] : [];
    return [base > 0 && more(base, "lg:hidden"), lg > 0 && more(lg, "hidden lg:list-item")].filter(Boolean);
  };
  const entityItems = (kind: string, entities: EntityChip[], total: number) => [
    ...entities.map((entity, i) => <li key={`${kind}:${entity.id}`} data-kind={kind} className={atWidth(i)}><Chip url={entity.path} name={entity.name || entity.slug} variant={entityStyle} /></li>),
    ...overflowItems(kind, total),
  ];
  const [chipList, setChipList] = useState<HTMLUListElement | null>(null);
  const fitting = chipFit && entityLayout === "inline";
  const chipTotal = entityGroups.reduce((n, g) => n + g.total, 0);
  const chipsKey = `${entityStyle}:${limit.base}:${limit.lg}:${entityGroups.map(({ kind, entities }) => `${kind}=${entities.map((e) => e.id).join(",")}`).join(";")}`;
  const fit = useChipFit(fitting ? chipList : null, chipTotal, chipsKey);
  const fittedItems = () => {
    let index = 0;
    const items = entityGroups.flatMap(({ kind, entities }) => entities.map((entity, i) => {
      const at = index++;
      const out = fit !== null && at >= fit.through;
      const squeezed = fit?.squeezed && at === fit.through - 1;
      return <li key={`${kind}:${entity.id}`} data-kind={kind} className={cn(squeezed ? "min-w-0 shrink [&>*]:block [&>*]:truncate" : "shrink-0", atWidth(i), out && "pointer-events-none invisible absolute")} aria-hidden={out || undefined}><Chip url={entity.path} name={entity.name || entity.slug} variant={entityStyle} /></li>;
    }));
    if (!chipOverflow) return items;
    // Always present, so its width is kept free; shown once measured with chips left out.
    const more = fit === null ? chipTotal : chipTotal - fit.fitted;
    const hidden = fit === null || more <= 0;
    return [...items, <li key="more" data-ckui="card-chip-more" className={cn("shrink-0", hidden && "pointer-events-none invisible absolute")} aria-hidden={hidden || undefined}>
      <span aria-hidden><Chip name={`+${more}`} variant={entityStyle} /></span>
      <span className="sr-only">{m.plural("contents.moreChips", more)}</span>
    </li>];
  };
  const entityList = entityGroups.length > 0 ? entityLayout === "rows" ? (
    <div className="grid gap-1" data-ckui="card-entities" data-layout="rows">
      {entityGroups.map(({ kind, entities, total }) => <ul key={kind} className={entityClassName} aria-label={labelOf(labels, kind)} data-ckui="card-entity-row" data-kind={kind}>{entityItems(kind, entities, total)}</ul>)}
    </div>
  ) : (
    <ul ref={setChipList} className={fitting ? cn("relative flex flex-nowrap overflow-hidden", entityStyle === "text" ? "gap-x-2" : "gap-1") : entityClassName} data-ckui="card-entities" data-layout="inline" data-fit={fitting ? "" : undefined}>
      {fitting ? fittedItems() : entityGroups.flatMap(({ kind, entities, total }) => entityItems(kind, entities, total))}
    </ul>
  ) : null;
  const metaLine = meta.length > 0 && <p className="line-clamp-2 text-xs break-words text-muted-foreground" data-ckui="card-meta">
    {meta.map((item, i) => <Fragment key={i}>
      {i > 0 && <><span className="sr-only"> · </span><span className="mx-1.5 mb-px inline-block size-[3px] rounded-full bg-current align-middle" aria-hidden data-ckui="card-meta-dot" /></>}
      {item}
    </Fragment>)}
  </p>;
  const titleLink = <a href={url} onClick={onClick} className={cn("text-lg leading-snug font-medium break-words focus-visible:underline sm:text-xl", titlePosition === "cover" ? "absolute inset-x-0 bottom-0 z-10 bg-linear-to-t from-black/90 via-black/60 to-transparent px-3 pt-10 pb-3 text-white hover:underline" : "hover:underline")} data-ckui="card-title">
    <span className="line-clamp-2">{title}</span>
    {lock && <span className="sr-only"> ({lock})</span>}
    {dim && <span className="sr-only"> ({t("contents.disliked")})</span>}
  </a>;
  const [article, setArticle] = useState<HTMLElement | null>(null);
  return (
    <article ref={setArticle} className={cn("group/card relative grid content-start gap-2", className)} data-ckui="content-card" data-id={card.id} data-locked={lock ? "" : undefined} lang={card.language || undefined}>
      <div className="relative overflow-hidden rounded-lg" data-ckui="card-cover">
        <a href={url} onClick={onClick} className="relative block overflow-hidden rounded-lg bg-muted" tabIndex={-1} aria-hidden>
          <div data-ckui="card-cover-frame" data-disliked={dim || undefined} className={cn(coverAspect === undefined && (shape === "landscape" ? "aspect-video" : "aspect-[3/4]"), "transition-[opacity,filter]", dim && "opacity-50 brightness-75", st.deleted && "opacity-60 grayscale")} style={coverAspect === undefined ? undefined : { aspectRatio: coverAspect }}>
            {loadMedia ? <RenditionImg
              outputs={card.cover}
              alt=""
              fit="cover"
              skeleton
              loading="lazy"
              className="size-full transition-transform group-hover/card:scale-[1.02]"
              fallback={<span className="grid size-full place-items-center text-2xl font-semibold text-muted-foreground">{title.charAt(0).toUpperCase()}</span>}
            /> : <span className="block size-full bg-muted" aria-hidden />}
          </div>
          {loadMedia && v.hls && <HoverPreview item={{ kind: card.kind, id: card.id, version: v.id }} dir={v.hls} duration={v.duration_ms ? v.duration_ms / 1000 : undefined} target={article} />}
          {lock && (
            <span className="absolute top-2 left-2 inline-flex items-center gap-1 rounded-md bg-background/85 px-1.5 py-0.5 text-xs font-medium text-foreground shadow-sm" data-ckui="card-lock">
              <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-hidden />
              {lock}
            </span>
          )}
          {progress > 0 && (
            <span className={cn("absolute inset-x-0 bottom-0 h-1 bg-background/60", titlePosition === "cover" && "z-20")} data-ckui="card-progress">
              <span className="block h-full bg-primary" style={{ width: `${Math.round(progress * 100)}%` }} />
            </span>
          )}
        </a>
        {titlePosition === "cover" && titleLink}
        {status && <CardStatusBadges card={card} className={cn("absolute right-2 z-20", titlePosition === "cover" ? "top-2" : "bottom-2.5")} />}
        {loadMedia && <Decorations card={card} decorate={decorate} aboveTitle={titlePosition === "cover"} />}
      </div>
      <div className="grid gap-1">
        {titlePosition === "below" && titleLink}
        {entityStyle === "text" && entityList}
        {showChannel && card.channel && <ChannelChip channel={card.channel} href={channelHref} loadMedia={loadMedia} />}
        {traits.length > 0 && <div className="flex flex-wrap gap-1" data-ckui="card-traits">
          {traits.map((trait) => <Badge key={trait} variant="outline" className="h-auto rounded-md text-sm text-muted-foreground" data-ckui="card-trait" data-trait={trait}>{labelOf(labels, trait)}</Badge>)}
        </div>}
        {metaLine}
        {!card.access.allowed && <ReleaseNotice releases={card.access.releases} />}
        
        {entityStyle === "chips" && entityList}
        {showCounts && (card.counts.favorites !== undefined || card.counts.likes !== undefined) && (
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

/** The original channel, independent of where a post is reposted. */
export function ChannelChip({ channel, href, className, loadMedia = true }: { channel: ChannelLike; href?: (channel: ChannelLike) => string | undefined; className?: string; loadMedia?: boolean }) {
  return <span className={cn("flex min-w-0 text-xs text-muted-foreground", className)} data-ckui="card-channel" data-id={channel.id}>
    <ChannelLink channel={channel} href={href} className="inline-flex min-w-0 items-center gap-1.5 hover:text-foreground">
      {loadMedia ? <ChannelAvatar channel={channel} className="size-5 shrink-0 text-[0.625rem]" /> : <span className="size-5 shrink-0 rounded-full bg-muted" aria-hidden />}
      <span className="truncate">{channel.name || channel.slug}</span>
    </ChannelLink>
  </span>;
}

function MetaLink({ url, name }: { url?: string; name: string }) {
  const onClick = useLink(url);
  return url ? <a href={url} onClick={onClick} className="hover:text-foreground hover:underline">{name}</a> : <span>{name}</span>;
}

function Chip({ url, name, variant = "chips" }: { url?: string; name: string; variant?: "chips" | "text" }) {
  const onClick = useLink(url);
  const cls = variant === "text" ? "text-sm text-muted-foreground" : "inline-block rounded-md border border-border px-1.5 py-0.5 text-sm text-muted-foreground";
  return url ? (
    <a href={url} onClick={onClick} className={cn(cls, "hover:text-foreground", variant === "text" ? "hover:underline" : "hover:bg-muted")}>
      {name}
    </a>
  ) : (
    <span className={cls}>{name}</span>
  );
}

export interface PostCardProps extends Omit<ContentCardProps, "shape" | "aspectRatio" | "titlePosition" | "chips" | "entityKinds" | "filterEntity" | "entityStyle" | "entityLayout" | "metadata" | "cardTitle"> {
  /** Above the title: the post's channel or author; default its channel's chip. */
  byline?: ReactNode;
}

/** A post in a feed: a wide cover, its title, when it was published and what unlocking it costs. */
export function PostCard({ card, href, onOpen, tiers, labels, byline, showChannel = true, channelHref, loadMedia = true, showCounts = true, decorate, children, className }: PostCardProps) {
  const m = useMessages();
  const url = href ? href(card) : card.path;
  const open = useCallback(() => onOpen?.(card), [onOpen, card]);
  const onClick = useLink(url, open);
  const lock = lockLabel(m, card.access, tiers, labels);
  const title = card.title || card.code;
  return (
    <article className={cn("relative grid gap-3 rounded-lg border border-border p-3", className)} data-ckui="post-card" data-id={card.id} data-locked={lock ? "" : undefined} lang={card.language || undefined}>
      {byline ?? (showChannel && card.channel && <ChannelChip channel={card.channel} href={channelHref} loadMedia={loadMedia} className="text-sm" />)}
      <a href={url} onClick={onClick} className="grid gap-2">
        <span className="font-medium break-words hover:underline" data-ckui="card-title">
          {title}
        </span>
        {card.cover.length > 0 && (
          <span className="relative block aspect-video overflow-hidden rounded-md bg-muted">
            {loadMedia && <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton loading="lazy" className={cn("size-full", lock && "blur-md")} />}
            {loadMedia && <Decorations card={card} decorate={decorate} />}
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
        {showCounts && card.counts.favorites !== undefined && (
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

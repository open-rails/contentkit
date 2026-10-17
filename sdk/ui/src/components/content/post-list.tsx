import { HugeiconsIcon } from "@hugeicons/react";
import { LockIcon } from "@hugeicons/core-free-icons";
import { cn } from "cn";
import { useMemo, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Period } from "../../client/discovery.js";
import type { Card } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useContentDetail } from "../../react/detail.js";
import { useContents } from "../../react/discovery.js";
import { usePostReads } from "../../react/post-reads.js";
import { useNearViewport } from "../../react/viewport.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { RenditionImg } from "../rendition-img.js";
import { RelativeTime } from "../social/parts.js";
import { lockLabel, type PostCardProps } from "./card.js";

export interface PostListProps {
  /** The post kind. */
  kind: string;
  /** Display language for titles, excerpts and paths; default the provider's. */
  language?: string;
  href?: (card: Card) => string;
  /** Posts per page; default 10. */
  pageSize?: number;
  /** Trending window for the sidebar, or false to leave it out; default 30d. */
  trending?: Period | false;
  /** Posts per sidebar section; default 5, 0 hides the sidebar. */
  sidebarSize?: number;
  /** Excerpt length in characters; default 220. */
  excerptLength?: number;
  tiers?: readonly string[];
  labels?: PostCardProps["labels"];
  empty?: ReactNode;
  client?: ContentKitClient;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

/**
 * Posts newest first with cover, date, excerpt and an unread mark, beside a
 * Newest/Trending sidebar. Trending is the kind's popular window; it never
 * counts views.
 */
export function PostList({ kind, language, href, pageSize = 10, trending = "30d", sidebarSize = 5, excerptLength = 220, tiers, labels, empty, client, appearance, className }: PostListProps) {
  const m = useMessages();
  const { t } = m;
  const list = useContents(useMemo(() => ({ kind, sort: "newest" as const, limit: pageSize, display_language: language }), [kind, pageSize, language]), { client });
  const reads = usePostReads();
  const url = (card: Card) => (href ? href(card) : card.path);
  const sidebar = sidebarSize > 0;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-6 text-sm", sidebar && "lg:grid-cols-[minmax(0,1fr)_18rem]", className)} data-ckui="post-list">
      <div className="grid min-w-0 content-start gap-3">
        {list.loading ? (
          <ol className="grid divide-y divide-border" aria-busy>
            {Array.from({ length: Math.min(pageSize, 4) }, (_, i) => (
              <li key={i} className="grid gap-4 py-4 sm:grid-cols-[14rem_minmax(0,1fr)]">
                <Skeleton className="aspect-[331/232] w-full rounded-lg" />
                <div className="grid content-start gap-2">
                  <Skeleton className="h-5 w-3/4" />
                  <Skeleton className="h-3 w-24" />
                  <Skeleton className="h-3 w-full" />
                  <Skeleton className="h-3 w-5/6" />
                </div>
              </li>
            ))}
          </ol>
        ) : list.items.length === 0 ? (
          <p className="text-muted-foreground">{empty ?? t("contents.empty")}</p>
        ) : (
          <ol className="grid divide-y divide-border">
            {list.items.map((card) => (
              <PostRow
                key={card.id}
                card={card}
                url={url(card)}
                unread={reads.unread(card.id, card.published_at, card.viewer?.seen || card.viewer?.completed)}
                language={language}
                excerptLength={excerptLength}
                lock={lockLabel(m, card.access, tiers, labels)}
                client={client}
              />
            ))}
          </ol>
        )}
        {list.hasMore && (
          <Button variant="outline" className="justify-self-center" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.loadingMore ? t("contents.loading") : t("contents.loadMore")}
          </Button>
        )}
      </div>
      {sidebar && (
        <aside className="grid content-start gap-6" data-ckui="post-list-sidebar">
          <SidebarSection title={t("postList.newest")} kind={kind} sort="newest" size={sidebarSize} language={language} url={url} client={client} />
          {trending && <SidebarSection title={t("postList.trending")} kind={kind} sort={`popular:${trending}`} size={sidebarSize} language={language} url={url} client={client} />}
        </aside>
      )}
    </ContentKitUiRoot>
  );
}

function PostRow({ card, url, unread, lock, language, excerptLength, client }: { card: Card; url: string; unread: boolean; lock: string | null; language?: string; excerptLength: number; client?: ContentKitClient }) {
  const { t } = useMessages();
  const [ref, near] = useNearViewport();
  return (
    <li ref={ref} className="grid gap-4 py-4 sm:grid-cols-[14rem_minmax(0,1fr)]" data-ckui="post-row" data-id={card.id} data-unread={unread || undefined} lang={card.language || undefined}>
      {card.cover.length > 0 && (
        <a href={url} className="relative block aspect-[331/232] overflow-hidden rounded-lg bg-muted" tabIndex={-1} aria-hidden>
          <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton loading="lazy" className={cn("size-full", lock && "blur-md")} />
        </a>
      )}
      <div className={cn("grid min-w-0 content-start gap-1.5", card.cover.length === 0 && "sm:col-span-2")}>
        <a href={url} className="flex items-start gap-2 text-lg leading-snug font-semibold break-words hover:underline">
          {unread && (
            <span className="mt-2 size-2 shrink-0 rounded-full bg-primary" data-ckui="post-unread">
              <span className="sr-only">{t("postList.unread")}</span>
            </span>
          )}
          <span>{card.title || card.code}</span>
        </a>
        <p className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
          {card.published_at && <RelativeTime at={card.published_at} />}
          {lock && (
            <span className="inline-flex items-center gap-1" data-ckui="card-lock">
              <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-hidden />
              {lock}
            </span>
          )}
        </p>
        {near ? <PostExcerpt id={card.id} language={language} length={excerptLength} client={client} /> : <span className="block h-10" />}
      </div>
    </li>
  );
}

function PostExcerpt({ id, language, length, client }: { id: string; language?: string; length: number; client?: ContentKitClient }) {
  const detail = useContentDetail(id, { language, client, version: null });
  if (detail.loading) {
    return (
      <span className="grid gap-1.5" aria-hidden>
        <Skeleton className="h-3 w-full" />
        <Skeleton className="h-3 w-4/5" />
      </span>
    );
  }
  const text = excerpt(detail.content?.description || plainText(detail.content?.body ?? ""), length);
  return text ? <p className="line-clamp-3 leading-relaxed text-muted-foreground">{text}</p> : null;
}

function SidebarSection({ title, kind, sort, size, language, url, client }: { title: string; kind: string; sort: "newest" | `popular:${Period}`; size: number; language?: string; url: (card: Card) => string; client?: ContentKitClient }) {
  const list = useContents(useMemo(() => ({ kind, sort, limit: size, display_language: language }), [kind, sort, size, language]), { client });
  if (!list.loading && list.items.length === 0) return null;
  return (
    <section className="grid content-start gap-3" aria-label={title}>
      <h2 className="text-base font-semibold">{title}</h2>
      <ol className="grid gap-3">
        {list.loading
          ? Array.from({ length: Math.min(size, 3) }, (_, i) => <Skeleton key={i} className="h-14 w-full" />)
          : list.items.slice(0, size).map((card) => (
              <li key={card.id}>
                <a href={url(card)} className={cn("grid items-center gap-3 hover:underline", card.cover.length > 0 && "grid-cols-[5rem_minmax(0,1fr)]")}>
                  {card.cover.length > 0 && (
                    <span className="block aspect-[3/2] overflow-hidden rounded-md bg-muted">
                      <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton loading="lazy" className={cn("size-full", !card.access.allowed && "blur-sm")} />
                    </span>
                  )}
                  <span className="grid min-w-0 gap-0.5">
                    <span className="line-clamp-2 font-medium break-words">{card.title || card.code}</span>
                    {card.published_at && <RelativeTime at={card.published_at} className="text-xs text-muted-foreground" />}
                  </span>
                </a>
              </li>
            ))}
      </ol>
    </section>
  );
}

/** Text without markup, whitespace collapsed. */
export function plainText(html: string): string {
  if (!html) return "";
  if (typeof DOMParser === "function") return (new DOMParser().parseFromString(html, "text/html").body.textContent ?? "").replace(/\s+/g, " ").trim();
  return html.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim();
}

/** At most length characters, cut at a word boundary with an ellipsis. */
export function excerpt(text: string, length: number): string {
  const s = text.replace(/\s+/g, " ").trim();
  if (s.length <= length) return s;
  const cut = s.slice(0, length + 1);
  const space = cut.lastIndexOf(" ");
  return `${(space > length * 0.6 ? cut.slice(0, space) : s.slice(0, length)).replace(/[\s.,;:!?-]+$/, "")}…`;
}

import { HugeiconsIcon } from "@hugeicons/react";
import { Image01Icon, LockIcon } from "@hugeicons/core-free-icons";
import { cn } from "cn";
import { useMemo, useRef, type CSSProperties, type ReactNode, type Ref } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { ratio } from "../../client/aspect.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentQuery, Period } from "../../client/discovery.js";
import type { Card } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useContents, useContentsPage } from "../../react/discovery.js";
import { usePostReads } from "../../react/post-reads.js";
import { usePresets } from "../../react/public.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { RenditionImg } from "../rendition-img.js";
import { lockLabel, type PostCardProps } from "./card.js";
import { usePage } from "./paged-grid.js";
import { InAppLink } from "./in-app-link.js";
import { Pagination } from "./pagination.js";

export interface PostListProps {
  /** The post kind. */
  kind: string;
  /** Display language for titles, excerpts and paths; default the provider's. */
  language?: string;
  href?: (card: Card) => string;
  /** Posts per page; default 10. */
  pageSize?: number;
  /** Numbered pages with the selection total, instead of "Load more". */
  pagination?: "pages";
  /** The numbered page, from 1; with onPageChange the host keeps it (in its URL). */
  page?: number;
  onPageChange?: (page: number) => void;
  /** Each page's link, for crawlers and opening in a new tab. */
  pageHref?: (page: number) => string;
  /** A CSS aspect ratio for covers; default the kind's cover preset, else 331 / 232. */
  aspectRatio?: CSSProperties["aspectRatio"];
  /** Trending window for the sidebar, or false to leave it out; default 30d. */
  trending?: Period | false;
  /** Posts per sidebar section; default 5, 0 hides the sidebar. */
  sidebarSize?: number;
  /** Excerpt length in characters; default 220. */
  excerptLength?: number;
  /** Signed in, posts unread within this many days are marked; default 30. */
  unreadDays?: number;
  tiers?: readonly string[];
  labels?: PostCardProps["labels"];
  empty?: ReactNode;
  client?: ContentKitClient;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

const LEGACY_POST_COVER = "331 / 232";

/** The kind's cover shape: the host's, else its cover preset's, else the legacy post cover. */
export function usePostCoverAspect(kind: string, aspectRatio?: CSSProperties["aspectRatio"], client?: ContentKitClient): CSSProperties["aspectRatio"] {
  const { presets } = usePresets({ client });
  return aspectRatio ?? ratio(presets.find((p) => p.kind === kind && p.name === "cover")?.aspect) ?? LEGACY_POST_COVER;
}

/**
 * Posts newest first with cover, date, excerpt and an unread mark, beside a
 * Newest/Trending sidebar. Trending is the kind's popular window; it never
 * counts views. With pagination="pages" the list is numbered pages, the page
 * kept by the host (page/onPageChange) or by the list.
 */
export function PostList(p: PostListProps) {
  return p.pagination === "pages" ? <PagedPosts {...p} /> : <CursorPosts {...p} />;
}

function CursorPosts(p: PostListProps) {
  const { t } = useMessages();
  const { kind, language, pageSize = 10, client } = p;
  const list = useContents(useMemo(() => ({ kind, sort: "newest" as const, limit: pageSize, display_language: language }), [kind, pageSize, language]), { client });
  return (
    <PostListLayout {...p} items={list.items} loading={list.loading}>
      {list.hasMore && (
        <Button variant="outline" className="justify-self-center" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
          {list.loadingMore ? t("contents.loading") : t("contents.loadMore")}
        </Button>
      )}
    </PostListLayout>
  );
}

function PagedPosts(p: PostListProps) {
  const { kind, language, pageSize = 10, client } = p;
  const query = useMemo<ContentQuery>(() => ({ kind, sort: "newest", limit: pageSize, display_language: language, summary: true }), [kind, pageSize, language]);
  const [page, setPage] = usePage(p, query);
  const result = useContentsPage(query, page, { client });
  const top = useRef<HTMLDivElement>(null);
  const total = result.summary?.total ?? 0;
  const controls = (bottom: boolean) =>
    !result.loading || total > 0 ? (
      <Pagination
        page={page}
        total={total}
        atLeast={result.summary?.truncated}
        pageSize={pageSize}
        pageHref={p.pageHref}
        onPageChange={(next) => {
          setPage(next);
          if (bottom) top.current?.scrollIntoView({ block: "start", behavior: "smooth" });
        }}
      />
    ) : null;
  return (
    <PostListLayout {...p} items={result.items} loading={result.loading} listRef={top} header={controls(false)}>
      {result.items.length > 0 && controls(true)}
    </PostListLayout>
  );
}

function PostListLayout({ kind, language, href, pageSize = 10, aspectRatio, trending = "30d", sidebarSize = 5, excerptLength = 220, unreadDays = 30, tiers, labels, empty, client, appearance, className, items, loading, listRef, header, children }: PostListProps & { items: Card[]; loading: boolean; listRef?: Ref<HTMLDivElement>; header?: ReactNode; children?: ReactNode }) {
  const m = useMessages();
  const { t } = m;
  const reads = usePostReads(unreadDays);
  const aspect = usePostCoverAspect(kind, aspectRatio, client);
  const url = (card: Card) => (href ? href(card) : card.path);
  const sidebar = sidebarSize > 0;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-6 text-sm", sidebar && "lg:grid-cols-[minmax(0,1fr)_18rem]", className)} data-ckui="post-list">
      <div ref={listRef} className="grid min-w-0 scroll-mt-20 content-start gap-3">
        {header}
        {loading ? (
          <ol className="grid divide-y divide-border" aria-busy>
            {Array.from({ length: Math.min(pageSize, 4) }, (_, i) => (
              <li key={i} className="grid gap-4 py-4 sm:grid-cols-[14rem_minmax(0,1fr)]">
                <Skeleton className="w-full rounded-lg" style={{ aspectRatio: aspect }} />
                <div className="grid content-start gap-2">
                  <Skeleton className="h-5 w-3/4" />
                  <Skeleton className="h-3 w-24" />
                  <Skeleton className="h-3 w-full" />
                  <Skeleton className="h-3 w-5/6" />
                </div>
              </li>
            ))}
          </ol>
        ) : items.length === 0 ? (
          <p className="text-muted-foreground">{empty ?? t("contents.empty")}</p>
        ) : (
          <ol className="grid divide-y divide-border">
            {items.map((card) => (
              <PostRow
                key={card.id}
                card={card}
                url={url(card)}
                aspect={aspect}
                unread={reads.unread(card.id, card.published_at, card.viewer?.seen || card.viewer?.completed)}
                excerptLength={excerptLength}
                lock={lockLabel(m, card.access, tiers, labels)}
              />
            ))}
          </ol>
        )}
        {children}
      </div>
      {sidebar && (
        <aside className="grid content-start gap-6" data-ckui="post-list-sidebar">
          <SidebarSection title={t("postList.newest")} kind={kind} sort="newest" size={sidebarSize} language={language} url={url} aspect={aspect} client={client} />
          {trending && <SidebarSection title={t("postList.trending")} kind={kind} sort={`popular:${trending}`} size={sidebarSize} language={language} url={url} aspect={aspect} client={client} />}
        </aside>
      )}
    </ContentKitUiRoot>
  );
}

function PostRow({ card, url, aspect, unread, lock, excerptLength }: { card: Card; url: string; aspect: CSSProperties["aspectRatio"]; unread: boolean; lock: string | null; excerptLength: number }) {
  const { t } = useMessages();
  const text = excerpt(card.excerpt ?? "", excerptLength);
  return (
    <li className="grid gap-4 py-4 sm:grid-cols-[14rem_minmax(0,1fr)]" data-ckui="post-row" data-id={card.id} data-unread={unread || undefined} lang={card.language || undefined}>
      <InAppLink href={url} className="relative block overflow-hidden rounded-lg bg-muted" style={{ aspectRatio: aspect }} tabIndex={-1} aria-hidden>
        {card.cover.length > 0 ? <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton loading="lazy" className={cn("size-full", lock && "blur-md")} /> : <NoCover className="size-8" />}
      </InAppLink>
      <div className="grid min-w-0 content-start gap-1.5">
        <InAppLink href={url} className="flex items-start gap-2 text-lg leading-snug font-semibold break-words hover:underline">
          {unread && (
            <span className="mt-2 size-2 shrink-0 rounded-full bg-primary" data-ckui="post-unread">
              <span className="sr-only">{t("postList.unread")}</span>
            </span>
          )}
          <span>{card.title || card.code}</span>
        </InAppLink>
        <p className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
          {card.published_at && <PublishedDate at={card.published_at} />}
          {lock && (
            <span className="inline-flex items-center gap-1" data-ckui="card-lock">
              <HugeiconsIcon icon={LockIcon} strokeWidth={2} className="size-3.5" aria-hidden />
              {lock}
            </span>
          )}
        </p>
        {text && <p className="line-clamp-3 leading-relaxed text-muted-foreground">{text}</p>}
      </div>
    </li>
  );
}

/** A post without a cover keeps its thumbnail slot, filled with a quiet image mark. */
function NoCover({ className }: { className: string }) {
  return (
    <span aria-hidden className="grid size-full place-items-center text-muted-foreground" data-ckui="post-no-cover">
      <HugeiconsIcon icon={Image01Icon} strokeWidth={1.5} className={className} />
    </span>
  );
}

function SidebarSection({ title, kind, sort, size, language, url, aspect, client }: { title: string; kind: string; sort: "newest" | `popular:${Period}`; size: number; language?: string; url: (card: Card) => string; aspect: CSSProperties["aspectRatio"]; client?: ContentKitClient }) {
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
                <InAppLink href={url(card)} className="grid grid-cols-[5rem_minmax(0,1fr)] items-center gap-3 hover:underline">
                  <span className="block overflow-hidden rounded-md bg-muted" style={{ aspectRatio: aspect }}>
                    {card.cover.length > 0 ? <RenditionImg outputs={card.cover} alt="" fit="cover" skeleton loading="lazy" className={cn("size-full", !card.access.allowed && "blur-sm")} /> : <NoCover className="size-5" />}
                  </span>
                  <span className="grid min-w-0 gap-0.5">
                    <span className="line-clamp-2 font-medium break-words">{card.title || card.code}</span>
                    {card.published_at && <PublishedDate at={card.published_at} className="text-xs text-muted-foreground" />}
                  </span>
                </InAppLink>
              </li>
            ))}
      </ol>
    </section>
  );
}

/** At most length characters, cut at a word boundary with an ellipsis. */
export function excerpt(text: string, length: number): string {
  const s = text.replace(/\s+/g, " ").trim();
  if (s.length <= length) return s;
  const cut = s.slice(0, length + 1);
  const space = cut.lastIndexOf(" ");
  return `${(space > length * 0.6 ? cut.slice(0, space) : s.slice(0, length)).replace(/[\s.,;:!?-]+$/, "")}…`;
}

/** A post's publication day, in the same style as release dates. */
function PublishedDate({ at, className }: { at: string; className?: string }) {
  const m = useMessages();
  return (
    <time dateTime={at} className={className}>
      {new Intl.DateTimeFormat(m.language, { dateStyle: "medium", timeZone: "UTC" }).format(new Date(at))}
    </time>
  );
}

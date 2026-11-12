import { cn } from "cn";
import { useSyncExternalStore, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Card, Content, Version } from "../../client/generated/wire.js";
import { resolveInlineImageReferences } from "../../client/inline-images.js";
import { useMessages } from "../../i18n/context.js";
import { usePostReading } from "../../react/post-reads.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Comments } from "../social/comments.js";
import { ReactionButtons } from "../social/engagement.js";
import { ContentRail } from "./grid.js";
import { InAppLink } from "./in-app-link.js";

export interface PostArticleProps {
  /** The post, as useContentDetail reads it. */
  content: Content;
  /** The version shown; its reading marks the post read. */
  version?: Version | null;
  /** The post list's link; omitted, no back link. */
  backHref?: string;
  backLabel?: ReactNode;
  /** The byline's publication; default the post's channel. */
  publication?: ReactNode;
  /** The body; default the stored HTML (sanitized on write) with its inline images. */
  body?: ReactNode;
  /** Newest posts of the kind beside the article; default 6, 0 leaves them out. */
  relatedSize?: number;
  relatedTitle?: ReactNode;
  /** Display language for the related posts; default the provider's. */
  language?: string;
  href?: (card: Card) => string;
  /** Reactions and comments under the body; default true. */
  engagement?: boolean;
  client?: ContentKitClient;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

const subscribeToHydration = () => () => {};
const clientSnapshot = () => true;
const serverSnapshot = () => false;

/**
 * One post to read: back link, title, summary, byline, cover, body, reactions
 * and comments, beside the kind's newest posts. Measured attention on the body
 * marks it read; an open alone does not.
 */
export function PostArticle({ content, version, backHref, backLabel, publication, body, relatedSize = 6, relatedTitle, language, href, engagement = true, client, appearance, className }: PostArticleProps) {
  const m = useMessages();
  const { t } = m;
  const readRef = usePostReading(content.id, version?.id, { client, kind: content.kind });
  const hydrated = useSyncExternalStore(subscribeToHydration, clientSnapshot, serverSnapshot);
  const cover = content.cover?.url;
  const day = (iso: string) => new Intl.DateTimeFormat(m.language, { dateStyle: "medium", timeZone: "UTC" }).format(new Date(iso));
  const url = (card: Card) => (href ? href(card) : card.path);
  const html = content.body ?? "";
  return (
    <ContentKitUiRoot
      appearance={appearance}
      className={cn("mx-auto grid w-full max-w-[1280px] py-[clamp(2rem,5vw,4rem)] xl:grid-cols-[minmax(0,1fr)_260px] xl:items-start xl:gap-12", (relatedSize <= 0) && "xl:grid-cols-1", "xl:[&:has([data-ckui=post-related]:empty)]:grid-cols-1", className)}
      data-ckui="post-article"
    >
      <article className="mx-auto w-full max-w-[960px] min-w-0 [overflow-wrap:anywhere]" lang={content.language || undefined}>
        <header className="mx-auto w-full max-w-[760px]">
          {backHref && (
            <InAppLink href={backHref} className="inline-flex items-center gap-2 text-sm text-muted-foreground no-underline transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-[5px] focus-visible:outline-ring">
              <span aria-hidden>←</span> {backLabel ?? t("postArticle.back")}
            </InAppLink>
          )}
          <h1 className="mt-5 mb-5 text-[clamp(2rem,4.5vw,3rem)] leading-[1.15] font-[750] tracking-[-0.035em] text-balance sm:mt-7">{content.title}</h1>
          {content.description && <p className="mb-6 text-[clamp(1.125rem,2vw,1.375rem)] leading-[1.55] text-muted-foreground">{content.description}</p>}
          <div className="flex flex-wrap items-center gap-2 text-sm leading-[1.6] text-muted-foreground">
            <span className="font-medium text-foreground">{publication ?? content.channel?.name}</span>
            {content.published_at && (
              <>
                <span aria-hidden>·</span>
                <time dateTime={content.published_at}>{day(content.published_at)}</time>
              </>
            )}
          </div>
        </header>
        {cover && (
          <figure className="my-7 sm:my-10">
            <img src={cover} alt="" fetchPriority="high" className="block max-h-[36rem] w-full rounded-lg bg-muted object-contain" />
          </figure>
        )}
        <div ref={readRef} className="mx-auto mt-7 w-full max-w-[760px] sm:mt-10" data-ckui="post-body">
          {body ?? (
            <div
              className="text-foreground"
              dangerouslySetInnerHTML={{ __html: hydrated ? resolveInlineImageReferences(html, content.images ?? {}) : html }}
            />
          )}
        </div>
        {engagement && (
          <>
            <footer className="mx-auto mt-10 flex w-full max-w-[760px] items-center justify-between gap-4 border-y border-border py-5">
              <ReactionButtons item={{ type: "contents", id: content.id }} client={client} />
            </footer>
            <div className="mx-auto mt-10 mb-12 w-full max-w-[760px]">
              <Comments item={{ type: "contents", id: content.id }} language={content.language} client={client} />
            </div>
          </>
        )}
      </article>
      {relatedSize > 0 && (
        <aside
          className="mt-4 min-w-0 border-t border-border pt-8 empty:hidden xl:mt-14 xl:border-t-0 xl:border-s xl:ps-6 xl:pt-0"
          aria-label={typeof relatedTitle === "string" ? relatedTitle : t("postArticle.related")}
          data-ckui="post-related"
        >
          <ContentRail
            title={relatedTitle ?? t("postArticle.related")}
            list={{ kind: content.kind, sort: "newest", language, limit: relatedSize }}
            grid={{ minCardWidth: 240, maxColumns: 1, mobileColumns: 1 }}
            href={url}
            client={client}
            renderCard={(card) => (
              <InAppLink
                href={url(card)}
                className="grid gap-2 py-3 text-[0.9375rem] leading-normal text-foreground [overflow-wrap:anywhere] hover:text-primary focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-ring"
                aria-current={card.id === content.id ? "page" : undefined}
              >
                <span className="line-clamp-3 font-semibold">{card.title || card.code}</span>
                {card.published_at && (
                  <time dateTime={card.published_at} className="text-[0.8125rem] text-muted-foreground">
                    {day(card.published_at)}
                  </time>
                )}
              </InAppLink>
            )}
          />
        </aside>
      )}
    </ContentKitUiRoot>
  );
}

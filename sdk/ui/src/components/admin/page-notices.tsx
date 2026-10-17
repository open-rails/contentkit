import { cn } from "cn";
import type { ContentKitClient } from "../../client/client.js";
import type { PageNotice } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { usePageNotices } from "../../react/page-notices.js";
import { Button } from "#ckui/ui/button";
import { ErrorLine, useAction } from "../social/parts.js";

export interface PageNoticesProps {
  content: string;
  version: string;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
}

const SHOWN = 8;
const REASON = { blank: "pageNotices.reason_blank", duplicate: "pageNotices.reason_duplicate", spread: "pageNotices.reason_spread" } as const;

/** Only link the editor media URLs supplied by the server. */
function imageURL(value?: string): string | undefined {
  if (!value) return;
  try {
    const url = new URL(value, globalThis.location?.href ?? "http://localhost/");
    if (url.protocol === "http:" || url.protocol === "https:") return value;
  } catch { /* An invalid or unsupported URL has no preview. */ }
}

/**
 * Low-key notes about what automatic page processing did: pages removed as
 * blank, duplicate or covered by a two-page spread (restorable), and an
 * archive put into name order. Collapsed by default; nothing here blocks
 * saving or publishing. Renders nothing without notices.
 */
export function PageNotices({ content, version, client, onError, className }: PageNoticesProps) {
  const m = useMessages();
  const { t } = m;
  const r = usePageNotices(content, version, { client });
  const act = useAction("reader.save", onError);
  if (!r.notices.length) return null;
  const names = (pages: PageNotice["pages"]) => {
    const shown = pages.slice(0, SHOWN).map((p) => p.source_name).join(", ");
    return pages.length > SHOWN ? `${shown} ${t("pageNotices.more", { count: pages.length - SHOWN })}` : shown;
  };
  return (
    <details className={cn("text-sm text-muted-foreground", className)} data-ckui="page-notices">
      <summary className="cursor-pointer select-none">{m.plural("pageNotices.notes", r.notices.length)}</summary>
      <ul className="mt-2 grid gap-3">
        {r.notices.map((n) => (
          <li key={n.id} className="grid gap-1.5" data-ckui="page-notice" data-kind={n.kind}>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <span className="text-foreground/80">{n.kind === "removed" ? m.plural("pageNotices.removed", n.pages.length) : t("pageNotices.reordered")}</span>
              <span className="ms-auto flex gap-1">
                {n.kind === "removed" && (
                  <Button type="button" size="xs" variant="ghost" disabled={act.pending} onClick={() => void act.run(() => r.restore(n.id))}>
                    {t("pageNotices.restore")}
                  </Button>
                )}
                <Button type="button" size="xs" variant="ghost" disabled={act.pending} onClick={() => void act.run(() => r.dismiss(n.id))}>
                  {t("pageNotices.dismiss")}
                </Button>
              </span>
            </div>
            {n.kind === "removed" ? (
              <ul className="flex flex-wrap gap-2">
                {n.pages.map((p) => {
                  const preview = imageURL(p.preview_url);
                  return (
                    <li key={p.id} className="grid w-16 gap-0.5 text-[11px]" title={p.source_name}>
                      {preview ? <img src={preview} alt="" loading="lazy" decoding="async" className="aspect-[3/4] w-16 rounded bg-muted object-cover opacity-80" /> : <span className="aspect-[3/4] w-16 rounded bg-muted" />}
                      <span className="truncate">{p.source_name}</span>
                      <span className="truncate">{t(REASON[p.reason as keyof typeof REASON] ?? REASON.blank)}</span>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 text-xs">
                <dt>{t("pageNotices.archiveOrder")}</dt>
                <dd className="truncate">{names([...n.pages].sort((a, b) => (a.from ?? 0) - (b.from ?? 0)))}</dd>
                <dt>{t("pageNotices.appliedOrder")}</dt>
                <dd className="truncate">{names(n.pages)}</dd>
              </dl>
            )}
          </li>
        ))}
      </ul>
      <ErrorLine error={act.error} />
    </details>
  );
}

import { cn } from "cn";
import { useState } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { PageReviewEntry, PageReviewPatch } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { usePageReview } from "../../react/page-review.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { SortableList } from "../sortable-list.js";
import { ErrorLine, useAction } from "../social/parts.js";
import { LoadFailed, Rows } from "./shell.js";

export interface PageReviewProps {
  content: string;
  version: string;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Only link the editor media URLs supplied by the server, never upload paths. */
function imageURL(value?: string): string | undefined {
  if (!value) return;
  try {
    const url = new URL(value, globalThis.location?.href ?? "http://localhost/");
    if (url.protocol === "http:" || url.protocol === "https:") return value;
  } catch { /* An invalid or unsupported URL has no preview. */ }
}

const words = (value: string) => value.replace(/[_-]+/g, " ");
const originalName = (page: PageReviewEntry) => page.source_entry || page.source_name;

/** Review automatic page choices without holding up ordinary publication. */
export function PageReview({ content, version, client, onError, className, appearance }: PageReviewProps) {
  const { t } = useMessages();
  const r = usePageReview(content, version, { client });
  const save = useAction("reader.save", onError);
  const [saved, setSaved] = useState(false);
  const review = r.review;
  const chapterGuarded = review?.pages.some((page) => page.flags.includes("segmented_selection_preserved")) ?? false;
  const apply = (patch: Omit<PageReviewPatch, "revision">) => {
    if (!review || chapterGuarded || save.pending || r.loading) return;
    setSaved(false);
    void save.run(() => r.update({ ...patch, revision: review.revision })).then((result) => result && setSaved(true));
  };
  const body = (() => {
    if (!review) return r.error ? <LoadFailed error={r.error} message={t("pageReview.loadFailed")} retry={r.reload} /> : <Rows count={2} />;
    if (!review.pages.length) return <p className="text-muted-foreground">{t("pageReview.empty")}</p>;
    const disabled = chapterGuarded || save.pending || r.loading;
    return <>
      <p className="text-muted-foreground">{t("pageReview.summary", { selected: review.selected, total: review.pages.length, flagged: review.flagged })}</p>
      {chapterGuarded
        ? <p role="status" className="rounded-md border border-border p-2 text-muted-foreground">{t("pageReview.chapterGuard")}</p>
        : <p className="text-xs text-muted-foreground">{t("pageReview.help")}</p>}
      <div className="max-h-[32rem] overflow-y-auto rounded-md border border-border p-2">
        <SortableList items={review.pages} id={(page) => page.id} name={originalName} label={t("pageReview.order")} disabled={disabled || review.pages.length < 2}
          onMove={(from, to) => {
            if (from === to) return;
            const order = review.pages.map((page) => page.id);
            const [moved] = order.splice(from, 1);
            order.splice(to, 0, moved!);
            apply({ order });
          }}>
          {(page, handle, index) => {
            const preview = imageURL(page.preview_url);
            const source = imageURL(page.source_url);
            return <div className="flex gap-2 rounded-md border border-border p-2" data-ckui="page-review-row" data-included={page.included} data-mode={page.mode}>
              <div className="flex shrink-0 flex-col items-center gap-1">
                <span className="text-xs text-muted-foreground">{index + 1}</span>
                {handle}
                {preview && <img src={preview} alt="" loading="lazy" decoding="async" className="h-16 w-12 rounded object-contain" />}
              </div>
              <div className="grid min-w-0 flex-1 gap-1">
                <p className="break-words"><span className="text-muted-foreground">{t("pageReview.original")} </span>{originalName(page)}</p>
                <p className="break-words"><span className="text-muted-foreground">{t("pageReview.canonical")} </span>{page.id}</p>
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant={page.included ? "outline" : "secondary"}>{t(page.included ? "pageReview.included" : "pageReview.excluded")}</Badge>
                  {!!page.flags.length && <Badge variant="outline">{t("pageReview.flagged")}</Badge>}
                  {source && <a href={source} target="_blank" rel="noopener noreferrer" className="text-xs underline">{t("pageReview.source")}</a>}
                </div>
                <p className="text-xs text-muted-foreground">{words(page.decision.reason)} · {t("pageReview.confidence", { confidence: words(page.decision.confidence) })}</p>
                {!!page.flags.length && <p className="text-xs text-muted-foreground">{page.flags.map(words).join(" · ")}</p>}
                <Select value={page.mode} onValueChange={(mode) => {
                  if (mode !== page.mode && (mode === "auto" || mode === "include" || mode === "exclude")) apply({ pages: [{ id: page.id, mode }] });
                }}>
                  <SelectTrigger size="sm" className="w-44" disabled={disabled} aria-label={t("pageReview.choice", { name: originalName(page) })}>
                    <SelectValue>{(mode: string) => t(mode === "include" ? "pageReview.include" : mode === "exclude" ? "pageReview.exclude" : "pageReview.automatic")}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="auto">{t("pageReview.automatic")}</SelectItem>
                    <SelectItem value="include">{t("pageReview.include")}</SelectItem>
                    <SelectItem value="exclude">{t("pageReview.exclude")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>;
          }}
        </SortableList>
      </div>
      {save.error?.status === 409 ? <p role="alert" className="text-destructive">{t("pageReview.conflict")}</p> : <ErrorLine error={save.error} />}
      {r.error && <LoadFailed error={r.error} message={t("pageReview.loadFailed")} retry={r.reload} />}
      {saved && <span role="status" className="text-muted-foreground">{t("pageReview.saved")}</span>}
    </>;
  })();
  return <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 text-sm", className)} data-ckui="page-review" aria-busy={save.pending || r.loading}>
    <div className="flex items-center justify-between gap-2">
      <h4 className="font-medium">{t("pageReview.title")}</h4>
      <Button type="button" size="xs" variant="ghost" disabled={save.pending || r.loading} onClick={r.reload}>{t("pageReview.refresh")}</Button>
    </div>
    {body}
  </ContentKitUiRoot>;
}

import { Alert02Icon, Loading03Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { ReactNode } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import type { ContentKitClient } from "../client/client.js";
import type { ReadResult, RefBody } from "../client/generated/wire.js";
import { isVideoType } from "../client/gallery.js";
import { isProcessing, processing } from "../client/media/windows.js";
import { useMessages } from "../i18n/context.js";
import { useMediaRead } from "../react/read.js";
import { ContentKitUiRoot } from "../scope.js";
import { Alert, AlertDescription, AlertTitle } from "#ckui/ui/alert";
import { Button } from "#ckui/ui/button";
import { Progress as ProgressBar } from "#ckui/ui/progress";
import { EncodeProgress } from "./encode-progress.js";

export interface MediaReadinessNoticeProps {
  /** The item; its editor read is polled while it processes. */
  item: RefBody;
  /** An editor read the host already has; otherwise fetched. */
  read?: ReadResult | null;
  /** Only uploads under this path prefix. */
  prefix?: string;
  /** Replaces the processing state's second line (e.g. who can see it meanwhile). */
  description?: ReactNode;
  className?: string;
  appearance?: ContentKitUiAppearance;
  client?: ContentKitClient;
}

/**
 * An item's processing state for its editors: nothing once every upload is
 * processed; while some process, a notice with the first video's encode
 * progress; when uploads failed, which ones (they hold the item back until
 * removed).
 */
export function MediaReadinessNotice({ item, read: given, prefix, description, className, appearance, client }: MediaReadinessNoticeProps) {
  const { t } = useMessages();
  const r = useMediaRead(item, { editor: true, status: given === undefined && !prefix, prefix, read: given, client });
  if (r.error) return <ContentKitUiRoot appearance={appearance} className={cn("w-full", className)} data-ckui="readiness" data-state="unavailable">
    <Alert variant="destructive" role="alert">
      <AlertTitle>{t("readiness.statusFailed")}</AlertTitle>
      <AlertDescription><Button size="sm" variant="outline" onClick={r.reload}>{t("common.retry")}</Button></AlertDescription>
    </Alert>
  </ContentKitUiRoot>;
  const uploads = (r.read?.files ?? []).filter((f) => f.upload && !f.unattached);
  const status = r.read?.status;
  const failures = status?.failures ?? uploads.filter((f) => f.failed).map((f) => ({ path: f.path, code: f.failed!.code, message: f.failed!.message }));
  const failureCount = status?.failure_count ?? failures.length;
  const full = !!r.read?.full || status?.state === "full";
  const failed = full || status?.state === "failed" || failureCount > 0;
  const busy = processing(r.read);
  if (!r.read || (!failed && !busy)) return null;
  const names = failures.map((f) => f.path.split("/").pop()!);
  const progress = status?.encoding ?? uploads.find((f) => isVideoType(f.type) && isProcessing(f) && f.progress)?.progress;
  const stages = status ? [
    ...(status.archives.total > 0 ? [{ key: "archives", label: t("readiness.archives"), ...status.archives }] : []),
    ...(status.archives.pages ? [{ key: "archive-pages", label: t("readiness.archivePages"), ...status.archives.pages }] : []),
    ...status.presets.map((preset) => ({ key: `preset:${preset.name}`, label: preset.name, ...preset })),
  ] : [];
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("w-full", className)} data-ckui="readiness" data-state={failed ? "failed" : "processing"}>
      <Alert variant={failed ? "destructive" : "default"} role={failed ? "alert" : "status"}>
        <HugeiconsIcon icon={failed ? Alert02Icon : Loading03Icon} strokeWidth={2} className={failed ? undefined : "motion-safe:animate-spin"} />
        <AlertTitle>{full ? t("readiness.full") : failed ? failureCount === 0 ? t("readiness.failedStatus") : failureCount === 1 ? t("readiness.failedOne", { name: names[0] ?? "" }) : t("readiness.failedMany", { count: failureCount, names: names.join(", ") }) : t("readiness.processing")}</AlertTitle>
        <AlertDescription className="grid gap-2">
          {description ?? t(full ? "readiness.fullHint" : failed ? "readiness.failedHint" : "readiness.processingHint")}
          {failures.length > 0 && <ul>{failures.map((failure, index) => <li key={`${failure.path}:${failure.code}:${index}`}>{failure.path}: {failure.message}</li>)}</ul>}
          {stages.map((stage) => {
            const label = t("readiness.stageProgress", { name: stage.label, completed: stage.completed, total: stage.total });
            return <div key={stage.key} className="grid gap-1" data-ckui="media-stage" data-stage={stage.key}>
              <p className="tabular-nums">{label}</p>
              <ProgressBar value={stage.total > 0 ? 100 * stage.completed / stage.total : null} aria-label={stage.label} aria-valuetext={label} />
            </div>;
          })}
          {progress && <EncodeProgress progress={progress} />}
        </AlertDescription>
      </Alert>
    </ContentKitUiRoot>
  );
}

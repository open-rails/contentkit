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
  const r = useMediaRead(item, { editor: true, prefix, read: given, client });
  const uploads = (r.read?.files ?? []).filter((f) => f.upload && !f.unattached);
  const failed = uploads.filter((f) => f.failed);
  const busy = processing(r.read);
  if (!r.read || (!failed.length && !busy)) return null;
  const names = failed.map((f) => f.path.split("/").pop()!);
  const progress = uploads.find((f) => isVideoType(f.type) && isProcessing(f) && f.progress)?.progress;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("w-full", className)} data-ckui="readiness" data-state={failed.length ? "failed" : "processing"}>
      {failed.length ? (
        <Alert variant="destructive" role="alert">
          <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
          <AlertTitle>{names.length === 1 ? t("readiness.failedOne", { name: names[0]! }) : t("readiness.failedMany", { count: names.length, names: names.join(", ") })}</AlertTitle>
          <AlertDescription>{t("readiness.failedHint")}</AlertDescription>
        </Alert>
      ) : (
        <Alert role="status">
          <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="motion-safe:animate-spin" />
          <AlertTitle>{t("readiness.processing")}</AlertTitle>
          <AlertDescription>
            {description ?? t("readiness.processingHint")}
            {progress && <EncodeProgress progress={progress} className="mt-2" />}
          </AlertDescription>
        </Alert>
      )}
    </ContentKitUiRoot>
  );
}

import { Download01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { FileDownload } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useVersionDownload } from "../../react/versions.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";

export interface VersionDownloadButtonProps {
  content: string;
  version: string;
  /** Hears each granted download. */
  onDownload?: (d: FileDownload) => void;
  /** Called instead of showing the locked notice (a paywall, sign in). */
  onLocked?: () => void;
  size?: "sm" | "default";
  variant?: "default" | "secondary" | "outline" | "ghost";
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/**
 * Downloads a version, as its kind serves it (the original video, a
 * rendition, its pages as a zip): the browser saves the file, charged to the
 * viewer's download quota. It waits while the file is prepared, says when a
 * quota refused and for how long, when the version is locked, and when the
 * site's download policy holds it back ("available to download in N days").
 */
export function VersionDownloadButton({ content, version, onDownload, onLocked, size = "default", variant = "secondary", client, onError, className, appearance }: VersionDownloadButtonProps) {
  const { t } = useMessages();
  const d = useVersionDownload(content, version, { client });
  const report = useErrorReporter(onError);
  // The clock the quota's countdown reads, kept while one holds.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (d.retryAt === undefined) return;
    const tick = () => setNow(Date.now());
    const first = setTimeout(tick, 0);
    const every = setInterval(tick, 15_000);
    return () => {
      clearTimeout(first);
      clearInterval(every);
    };
  }, [d.retryAt]);
  const left = d.retryAt === undefined ? 0 : d.retryAt - now;
  const limited = left > 0;
  const run = () => {
    d.download().then(onDownload, (err) => {
      if (err.code === "forbidden" && onLocked) return onLocked();
      report(err, "version.download");
    });
  };
  // A quota's notice counts down; a locked version goes to the host's onLocked.
  const notice = limited
    ? t("versionDownload.limited", { minutes: Math.max(1, Math.ceil(left / 60_000)) })
    : d.error?.code === "rate_limited" || (d.error?.code === "forbidden" && onLocked)
      ? undefined
      : d.message;
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("inline-flex flex-col items-start gap-1", className)} data-ckui="version-download">
      <Button variant={variant} size={size} disabled={d.preparing || limited} aria-busy={d.preparing} onClick={run}>
        <HugeiconsIcon icon={Download01Icon} strokeWidth={2} />
        <span>{d.preparing ? t("versionDownload.preparing") : t("versionDownload.label")}</span>
      </Button>
      {notice && (
        <p role="status" className="text-xs text-muted-foreground">
          {notice}
        </p>
      )}
    </ContentKitUiRoot>
  );
}

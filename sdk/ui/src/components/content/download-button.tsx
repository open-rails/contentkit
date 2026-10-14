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

/** "Available to download in N days", rounded up, for a refusal that opens later. */
function availableIn(at: string | undefined, now: number, plural: (key: string, count: number) => string): string | undefined {
  const ms = at ? Date.parse(at) - now : NaN;
  if (!(ms > 0)) return undefined;
  return plural("versionDownload.availableIn", Math.max(1, Math.ceil(ms / 86_400_000)));
}

/**
 * Downloads a version, as its kind serves it (the original video, a
 * rendition, its pages as a zip): the browser saves the file, charged to the
 * viewer's download quota. It waits while the file is prepared, says when a
 * quota refused and for how long, when the version is locked, and when the
 * site's download policy holds it back ("available to download in N days").
 */
export function VersionDownloadButton({ content, version, onDownload, onLocked, size = "default", variant = "secondary", client, onError, className, appearance }: VersionDownloadButtonProps) {
  const { t, plural, error: describe } = useMessages();
  const d = useVersionDownload(content, version, { client });
  const report = useErrorReporter(onError);
  const [now, setNow] = useState(() => Date.now());
  const limited = d.retryAt !== undefined && d.retryAt > now;
  useEffect(() => {
    if (!limited) return;
    const id = setTimeout(() => setNow(Date.now()), Math.min(d.retryAt! - Date.now(), 60_000));
    return () => clearTimeout(id);
  }, [limited, d.retryAt, now]);
  const run = () => {
    d.download().then(onDownload, (err) => {
      if (err.code === "forbidden" && onLocked) return onLocked();
      report(err, "version.download");
    });
  };
  let notice: string | undefined;
  if (limited) notice = t("versionDownload.limited", { minutes: Math.max(1, Math.ceil((d.retryAt! - now) / 60_000)) });
  else if (d.error?.code === "forbidden") notice = onLocked ? undefined : t("versionDownload.locked");
  else if (d.error?.code === "download_unavailable") notice = availableIn(d.error.availableAt, now, plural) ?? t("versionDownload.unavailable");
  else if (d.error?.code === "no_download") notice = describe(d.error);
  else if (d.error && d.error.code !== "rate_limited") notice = t("versionDownload.failed");
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

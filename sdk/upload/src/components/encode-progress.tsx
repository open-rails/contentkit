import { cn } from "cn";
import type { UploadUiAppearance } from "../appearance.js";
import { useMessages } from "../i18n/context.js";
import type { Translator } from "../i18n/messages.js";
import { useEncodeProgress } from "../react.js";
import { UploadUiRoot } from "../scope.js";
import type { EncodeProgress as Progress } from "../wire.gen.js";
import { Progress as Bar } from "#ckui/ui/progress";

export interface EncodeProgressProps {
  /** `files[i].progress` from the read API; absent renders a plain "Processing video" state. */
  progress?: Progress | null;
  className?: string;
  appearance?: UploadUiAppearance;
}

/** "~40 s left", "~3 min left", "~1 h 5 min left"; "almost done" under 5 s. */
export function formatRemaining(t: Translator["t"], seconds: number): string {
  if (seconds < 5) return t("encode.almostDone");
  if (seconds < 55) return t("encode.remainingSeconds", { seconds: seconds < 20 ? Math.ceil(seconds) : Math.ceil(seconds / 5) * 5 });
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return t("encode.remainingMinutes", { minutes });
  return t("encode.remainingHours", { hours: Math.floor(minutes / 60), minutes: minutes % 60 });
}

/** The status line: phase · segment n / N · time left, or the queue position. */
export function encodeLabel(t: Translator["t"], p: Progress | undefined, remaining?: number): string {
  if (!p) return t("encode.processing");
  if (p.stalled) return t("encode.stalled");
  // A second stage adds the rungs above 1080 to a video that already plays.
  const stage = p.stage && p.stage > 1 ? [t("encode.higherQualities")] : [];
  if (p.phase === "queued")
    return [...stage, p.queue_position ? t("encode.queuedPosition", { position: p.queue_position }) : t("encode.queued")].join(" · ");
  const parts = [...stage, t(`encode.phase.${p.phase}`)];
  if (p.phase === "encoding" && p.segments_total) parts.push(t("encode.segments", { done: p.segments_done ?? 0, total: p.segments_total }));
  if (remaining !== undefined) parts.push(formatRemaining(t, remaining));
  else if (p.phase === "encoding") parts.push(t("encode.estimating"));
  return parts.join(" · ");
}

/** A pending video's encode: progress bar, phase, segments and time left. Motion is limited to the bar's width, off under reduced motion. */
export function EncodeProgress({ progress, className, appearance }: EncodeProgressProps) {
  const { t } = useMessages();
  const { progress: p, remaining } = useEncodeProgress(progress);
  const label = encodeLabel(t, p, remaining);
  const value = p && p.percent > 0 ? Math.min(100, p.percent) : null;
  return (
    <UploadUiRoot appearance={appearance} className={cn("grid w-full gap-1.5 text-sm", className)} data-ckui="encode-progress" data-phase={p?.phase}>
      <p className="text-muted-foreground tabular-nums">
        {label}
      </p>
      <Bar
        value={value}
        aria-label={t("encode.processing")}
        aria-valuetext={value === null ? label : `${Math.round(value)}% · ${label}`}
        className="[&_[data-slot=progress-indicator]]:motion-reduce:transition-none [&[data-indeterminate]_[data-slot=progress-track]]:motion-safe:animate-pulse"
      />
    </UploadUiRoot>
  );
}

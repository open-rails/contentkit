import type { Progress } from "../client.js";
import type { Translator } from "../i18n/messages.js";

/** Hashing and uploading fill 0–90 %, completing and rendering the rest. */
export function progressValue(p: Progress | undefined, rendering?: boolean): number | null {
  if (rendering) return null;
  if (!p) return 0;
  const f = p.total > 0 ? p.loaded / p.total : 0;
  if (p.phase === "hashing") return Math.round(f * 10);
  if (p.phase === "uploading") return 10 + Math.round(f * 80);
  return 95;
}

export function progressLabel(t: Translator["t"], p: Progress | undefined, rendering?: boolean): string {
  if (rendering) return t("progress.rendering");
  if (!p) return t("progress.hashing");
  if (p.phase === "uploading") return t("progress.uploading", { percent: p.total > 0 ? Math.round((p.loaded / p.total) * 100) : 0 });
  return t(p.phase === "hashing" ? "progress.hashing" : "progress.completing");
}

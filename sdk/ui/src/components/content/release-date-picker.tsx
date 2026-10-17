import { useId } from "react";
import { cn } from "cn";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { ContentKitUiRoot } from "../../scope.js";

/** Today's YYYY-MM-DD in the browser's local calendar, without a UTC shift. */
export function localReleaseDate(now = new Date()): string {
  return `${String(now.getFullYear()).padStart(4, "0")}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
}

/** A real calendar date on or before max; empty selects the latest window. */
export function isReleaseDate(value: string, max = localReleaseDate()): boolean {
  if (!value) return true;
  if (!/^[0-9]{4}-[0-9]{2}-[0-9]{2}$/.test(value) || value.startsWith("0000") || value > max) return false;
  const at = new Date(`${value}T12:00:00Z`);
  return Number.isFinite(at.getTime()) && at.toISOString().slice(0, 10) === value;
}

export interface ReleaseDatePickerProps {
  /** Inclusive end_date of a DailyReleases query; empty means latest. */
  value?: string;
  onValueChange: (value: string) => void;
  /** YYYY-MM-DD in the listing's calendar. Browser-local today by default. */
  max?: string;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Native calendar with an editable year, so old dates need no month-by-month paging. */
export function ReleaseDatePicker({ value = "", onValueChange, max = localReleaseDate(), className, appearance }: ReleaseDatePickerProps) {
  const id = useId();
  const { t } = useMessages();
  const valid = isReleaseDate(value, max);
  return <ContentKitUiRoot appearance={appearance} className={cn("grid justify-items-start gap-2", className)} data-ckui="release-date-picker">
    <div className="flex flex-wrap items-center gap-3">
      <label htmlFor={id} className="text-base font-medium">{t("releases.throughDate")}</label>
      <Input id={id} type="date" min="0001-01-01" max={max} value={valid ? value : ""} className="h-11 w-auto min-w-40 text-base" aria-invalid={!valid || undefined} aria-describedby={!valid ? `${id}-error` : undefined} onChange={(event) => {
        const next = event.currentTarget.value;
        if (isReleaseDate(next, max)) onValueChange(next);
      }} />
      <Button type="button" variant="outline" disabled={!value} onClick={() => onValueChange("")}>{t("releases.latest")}</Button>
    </div>
    {!valid && <p id={`${id}-error`} role="alert" className="text-sm text-destructive">{t("releases.invalidDate")}</p>}
  </ContentKitUiRoot>;
}

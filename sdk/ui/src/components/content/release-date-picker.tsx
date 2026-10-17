import { Popover } from "@base-ui/react/popover";
import { Calendar01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useId, useState } from "react";
import { DayPicker } from "react-day-picker";
import { de, enUS, es, ja, ko, zhCN } from "react-day-picker/locale";
import { cn } from "cn";
import { Button } from "#ckui/ui/button";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { ContentKitUiRoot, useScopeProps } from "../../scope.js";

/** Today's YYYY-MM-DD in the browser's local calendar, without a UTC shift. */
export function localReleaseDate(now = new Date()): string {
  return `${String(now.getFullYear()).padStart(4, "0")}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
}

/** A real calendar date on or before max; empty leaves the feed's default date. */
export function isReleaseDate(value: string, max = localReleaseDate()): boolean {
  if (!value) return true;
  if (!/^[0-9]{4}-[0-9]{2}-[0-9]{2}$/.test(value) || value.startsWith("0000") || value > max) return false;
  const at = new Date(`${value}T12:00:00Z`);
  return Number.isFinite(at.getTime()) && at.toISOString().slice(0, 10) === value;
}

const calendarDate = (value: string) => new Date(`${value}T12:00:00`);
const locales = { de, en: enUS, es, ja, ko, zh: zhCN };
const navigationButton = "absolute top-0 flex size-10 items-center justify-center rounded-md border-0 bg-transparent text-inherit hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring aria-disabled:pointer-events-none aria-disabled:opacity-40";

export interface ReleaseDatePickerProps {
  /** Selected YYYY-MM-DD; empty leaves the feed's default date. */
  value?: string;
  onValueChange: (value: string) => void;
  /** Latest selectable YYYY-MM-DD in the listing's calendar. Local today by default. */
  max?: string;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A single-day calendar popover with month/year navigation and no typed input. */
export function ReleaseDatePicker({ value = "", onValueChange, max = localReleaseDate(), className, appearance }: ReleaseDatePickerProps) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const { t, language = "en" } = useMessages();
  const scope = useScopeProps(appearance);
  const limit = max && isReleaseDate(max, "9999-12-31") ? max : localReleaseDate();
  const valid = isReleaseDate(value, limit);
  const selected = value && valid ? calendarDate(value) : undefined;
  const locale = locales[language.split("-")[0] as keyof typeof locales] ?? enUS;
  const label = selected ? new Intl.DateTimeFormat(language, { dateStyle: "medium" }).format(selected) : t("releases.goToDay");
  return <Popover.Root open={open} onOpenChange={setOpen}>
    <ContentKitUiRoot appearance={appearance} className={cn("inline-flex flex-col items-end gap-2", className)} data-ckui="release-date-picker">
      <Popover.Trigger render={<Button type="button" variant="outline" className="h-11 gap-2 text-base" />} aria-label={t("releases.goToDay")} aria-invalid={!valid || undefined} aria-describedby={!valid ? `${id}-error` : undefined}>
        <HugeiconsIcon icon={Calendar01Icon} className="size-5" aria-hidden="true" />
        <span className="hidden sm:inline">{label}</span>
      </Popover.Trigger>
      {!valid && <p id={`${id}-error`} role="alert" className="text-sm text-destructive">{t("releases.invalidDate")}</p>}
    </ContentKitUiRoot>
    <Popover.Portal>
      <Popover.Positioner {...scope} className={cn(scope.className, "z-50")} align="end" sideOffset={8} collisionPadding={8}>
        <Popover.Popup initialFocus={false} aria-label={t("releases.goToDay")} className="rounded-xl border border-border bg-background p-3 text-foreground shadow-lg outline-none">
          <ContentKitUiRoot appearance={appearance} data-ckui="release-date-calendar">
            <DayPicker key={value || limit} mode="single" required selected={selected} defaultMonth={selected ?? calendarDate(limit)} startMonth={calendarDate("0001-01-01")} endMonth={calendarDate(limit)} disabled={{ after: calendarDate(limit) }} locale={locale} captionLayout="dropdown" navLayout="around" reverseYears autoFocus showOutsideDays onSelect={(date) => {
              const next = localReleaseDate(date);
              if (isReleaseDate(next, limit)) { onValueChange(next); setOpen(false); }
            }} classNames={{
              root: "w-fit",
              month: "relative space-y-3",
              month_caption: "flex h-10 items-center justify-center px-10",
              dropdowns: "flex items-center gap-2 text-sm font-medium",
              dropdown_root: "relative rounded-md focus-within:outline-2 focus-within:outline-ring",
              dropdown: "absolute inset-0 size-full cursor-pointer bg-popover text-popover-foreground opacity-0",
              caption_label: "flex items-center gap-1 whitespace-nowrap",
              button_previous: cn(navigationButton, "start-0"),
              button_next: cn(navigationButton, "end-0"),
              chevron: "size-4 fill-current",
              month_grid: "w-full border-collapse",
              weekday: "size-10 text-xs font-normal text-muted-foreground",
              day: "size-10 p-0 text-center",
              day_button: "size-10 rounded-md border-0 bg-transparent font-normal text-inherit outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none",
              selected: "[&>button]:bg-primary [&>button]:text-primary-foreground [&>button:hover]:bg-primary",
              today: "[&>button]:font-bold",
              outside: "text-muted-foreground",
              disabled: "opacity-40",
              hidden: "invisible",
            }} />
          </ContentKitUiRoot>
        </Popover.Popup>
      </Popover.Positioner>
    </Popover.Portal>
  </Popover.Root>;
}

import { Diamond02Icon, Settings01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { CSSProperties, MouseEvent } from "react";
import { ratio } from "../../client/aspect.js";
import type { Card } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { usePresets } from "../../react/public.js";
import { useNearViewport } from "../../react/viewport.js";
import { Button } from "#ckui/ui/button";
import { RenditionImg } from "../rendition-img.js";
import type { CardListProps } from "./grid.js";

/** One cover-sized control shared by complete lists and daily release sections. */
export function ContentSummaryTile({
  tileKey,
  kind,
  contentKind,
  preview,
  open,
  label,
  onClick,
  cardProps: p,
}: {
  tileKey: string;
  kind: "premium" | "filtered";
  contentKind?: string;
  preview?: Card;
  open: boolean;
  label: string;
  onClick: (event: MouseEvent<HTMLButtonElement>) => void;
  cardProps: CardListProps;
}) {
  const { t } = useMessages();
  const [ref, near] = useNearViewport({ margin: "0px", leaveMargin: "1200px 0px" });
  const { presets } = usePresets({ client: p.client });
  const aspect: CSSProperties["aspectRatio"] =
    p.aspectRatio ??
    ratio(presets.find((preset) => preset.kind === contentKind && preset.name === "cover")?.aspect) ??
    (p.shape === "landscape" ? "16 / 9" : "3 / 4");
  return (
    <li ref={ref} data-release-key={tileKey}>
      <Button
        type="button"
        variant="outline"
        data-ckui={`${kind}-release-tile`}
        aria-label={label}
        aria-expanded={kind === "premium" ? false : undefined}
        aria-haspopup={kind === "filtered" ? "dialog" : undefined}
        className={cn("relative flex h-auto w-full flex-col justify-center overflow-hidden rounded-lg p-0 text-center whitespace-normal", kind === "premium" && "bg-black text-white hover:bg-black hover:text-white")}
        style={{ aspectRatio: aspect }}
        onClick={onClick}
      >
        {preview && open && near && (
          <RenditionImg outputs={preview.cover} alt="" fit="cover" loading="lazy" className="absolute inset-0 size-full opacity-[0.22]" />
        )}
        <span className={cn("relative grid justify-items-center", kind === "premium" ? "gap-2.5 p-3 md:gap-4 md:p-6" : "gap-3 p-4")}>
          <HugeiconsIcon icon={kind === "premium" ? Diamond02Icon : Settings01Icon} strokeWidth={2} className={kind === "premium" ? "size-6 md:size-7" : "size-8"} aria-hidden />
          <span
            data-ckui="release-summary-label"
            data-stacked={kind === "premium" && label.includes("\n") || undefined}
            className={cn("font-semibold text-balance", kind === "premium" ? "text-sm leading-[1.4] whitespace-pre-line md:text-xl" : "text-xl")}
          >{label}</span>
          <span className={kind === "premium" ? "max-w-[14ch] text-xs leading-[1.4] text-balance opacity-80 md:max-w-[20ch] md:text-base" : "text-sm text-muted-foreground"}>{t(kind === "premium" ? "releases.premiumHint" : "releases.manageFilters")}</span>
        </span>
      </Button>
      {preview && <p className="mt-2 line-clamp-2 text-lg leading-snug text-muted-foreground">{preview.title}</p>}
    </li>
  );
}

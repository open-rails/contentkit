import { LockIcon, Settings01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
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
        className="relative flex h-auto w-full flex-col justify-center overflow-hidden rounded-lg p-0 text-center whitespace-normal"
        style={{ aspectRatio: aspect }}
        onClick={onClick}
      >
        {preview && open && near && (
          <RenditionImg outputs={preview.cover} alt="" fit="cover" loading="lazy" className="absolute inset-0 size-full opacity-20" />
        )}
        <span className="relative grid justify-items-center gap-3 p-4">
          <HugeiconsIcon icon={kind === "premium" ? LockIcon : Settings01Icon} strokeWidth={2} className="size-8" aria-hidden />
          <span className="text-xl font-semibold">{label}</span>
          <span className="text-sm text-muted-foreground">{t(kind === "premium" ? "releases.premiumHint" : "releases.manageFilters")}</span>
        </span>
      </Button>
      {preview && <p className="mt-2 line-clamp-2 text-lg leading-snug text-muted-foreground">{preview.title}</p>}
    </li>
  );
}

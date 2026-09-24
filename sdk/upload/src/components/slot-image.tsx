import { ratio, type AspectRatio } from "../aspect.js";
import { Image01Icon, UserIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { ComponentProps, ReactNode } from "react";
import type { UploadClient } from "../client.js";
import { useSlotImage } from "../slot-react.js";
import { manifestAspect } from "../srcset.js";
import type { DensityRange } from "../rendition.js";
import { RenditionImg } from "./rendition-img.js";
import type { RefBody, SlotManifest } from "../wire.gen.js";
import { useOptionalUploadClient } from "../provider.js";
import { UploadUiRoot } from "../scope.js";

export interface SlotImageProps extends Omit<ComponentProps<"img">, "src" | "srcSet" | "sizes" | "width" | "height" | "placeholder"> {
  /** A manifest the host already has; otherwise fetched with client.getSlot(item, slot). */
  manifest?: SlotManifest | null;
  item?: RefBody;
  slot?: string;
  client?: UploadClient;
  /** Density range for picking the rendition; default the provider's (2–3×). */
  density?: DensityRange;
  round?: boolean;
  /** Width / height of the box; default the manifest's aspect. */
  aspect?: AspectRatio;
  /** Shown when the slot is empty; default a muted box with an icon. */
  placeholder?: ReactNode;
  /** Accessible name of the empty state. */
  emptyLabel?: string;
}

/** A slot manifest's image, in a box at the slot's aspect, at the rendition its rendered width × density needs. */
export function SlotImage({ manifest, item, slot, client, density, round, aspect, placeholder, emptyLabel, className, alt = "", style, ...img }: SlotImageProps) {
  const c = useOptionalUploadClient(client);
  const fetched = useSlotImage(manifest === undefined && item && slot ? c : null, {
    ref: item ?? { kind: "", id: "" },
    slot: slot ?? "",
    manifest: manifest === undefined && item && slot ? undefined : (manifest ?? null),
  });
  const m = fetched.manifest;
  const has = !!m?.outputs.some((o) => o.url);
  const a = ratio(aspect ?? manifestAspect(m)) ?? 1;
  return (
    <UploadUiRoot
      className={cn("relative overflow-hidden bg-muted", round ? "rounded-full" : "rounded-lg", className)}
      style={{ aspectRatio: String(a), ...style }}
      data-ckui="slot-image"
      data-empty={has ? undefined : ""}
    >
      {has ? (
        <RenditionImg {...img} outputs={m!.outputs} density={density} alt={alt} className="absolute inset-0 size-full object-cover" />
      ) : (
        (placeholder ?? (
          <div role={emptyLabel ? "img" : undefined} aria-label={emptyLabel} className="absolute inset-0 flex items-center justify-center text-muted-foreground/70">
            <HugeiconsIcon icon={round ? UserIcon : Image01Icon} strokeWidth={1.5} className="size-1/3 max-h-10 max-w-10" />
          </div>
        ))
      )}
    </UploadUiRoot>
  );
}

import { Image01Icon, UserIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { ComponentProps, ReactNode } from "react";
import type { UploadClient } from "../client.js";
import { useSlotImage } from "../slot-react.js";
import { manifestAspect, slotSources } from "../srcset.js";
import type { RefBody, SlotManifest } from "../wire.gen.js";
import { useOptionalUploadClient } from "../provider.js";
import { UploadUiRoot } from "../scope.js";

export interface SlotImageProps extends Omit<ComponentProps<"img">, "src" | "srcSet" | "placeholder"> {
  /** A manifest the host already has; otherwise fetched with client.getSlot(item, slot). */
  manifest?: SlotManifest | null;
  item?: RefBody;
  slot?: string;
  client?: UploadClient;
  /** `sizes` for the srcset, e.g. "96px" or "(min-width: 768px) 720px, 100vw". Default "100vw". */
  sizes?: string;
  round?: boolean;
  /** Width / height of the box; default the manifest's aspect. */
  aspect?: number;
  /** Shown when the slot is empty; default a muted box with an icon. */
  placeholder?: ReactNode;
  /** Accessible name of the empty state. */
  emptyLabel?: string;
}

/** `<img srcset sizes>` from a slot manifest, in a box at the slot's aspect. */
export function SlotImage({ manifest, item, slot, client, sizes = "100vw", round, aspect, placeholder, emptyLabel, className, alt = "", style, ...img }: SlotImageProps) {
  const c = useOptionalUploadClient(client);
  const fetched = useSlotImage(manifest === undefined && item && slot ? c : null, {
    ref: item ?? { kind: "", id: "" },
    slot: slot ?? "",
    manifest: manifest === undefined && item && slot ? undefined : (manifest ?? null),
  });
  const m = fetched.manifest;
  const src = slotSources(m);
  const a = aspect ?? manifestAspect(m, 1);
  return (
    <UploadUiRoot
      className={cn("relative overflow-hidden bg-muted", round ? "rounded-full" : "rounded-lg", className)}
      style={{ aspectRatio: String(a), ...style }}
      data-ckui="slot-image"
      data-empty={src.src ? undefined : ""}
    >
      {src.src ? (
        <img
          {...img}
          alt={alt}
          src={src.src}
          srcSet={src.srcSet}
          sizes={sizes}
          width={src.width}
          height={src.height}
          decoding="async"
          className="absolute inset-0 size-full object-cover"
        />
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

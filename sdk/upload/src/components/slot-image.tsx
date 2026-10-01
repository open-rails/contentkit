import { ratio, type AspectRatio } from "../aspect.js";
import { Image01Icon, UserIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useState, type ComponentProps, type ReactNode } from "react";
import { publicRenditions, type PublicImage } from "../public.js";
import type { DensityRange } from "../rendition.js";
import { usePublicGeneration } from "../slot-react.js";
import { RenditionImg } from "./rendition-img.js";
import { UploadUiRoot } from "../scope.js";

export interface SlotImageProps extends Omit<ComponentProps<"img">, "src" | "srcSet" | "sizes" | "width" | "height" | "placeholder"> {
  /** The item's public preset (fixed URLs; a missing file is served its kind's default). */
  image?: PublicImage | null;
  /** Density range for picking the width; default the provider's (2–3×). */
  density?: DensityRange;
  round?: boolean;
  /** Width / height of the box; default the preset's aspect. */
  aspect?: AspectRatio;
  /** Shown without an image (or when it fails to load); default a muted box with an icon. */
  placeholder?: ReactNode;
  /** Accessible name of the empty state. */
  emptyLabel?: string;
}

/** A public image preset's file, in a box at its aspect, at the width its rendered width × density needs. */
export function SlotImage({ image, density, round, aspect, placeholder, emptyLabel, className, alt = "", style, onError, ...img }: SlotImageProps) {
  const generation = usePublicGeneration();
  const outputs = publicRenditions(image);
  const [failed, setFailed] = useState<string | null>(null);
  const key = `${outputs[0]?.url}#${generation}`;
  const has = outputs.length > 0 && failed !== key;
  const a = ratio(aspect ?? image?.aspect) ?? 1;
  return (
    <UploadUiRoot
      className={cn("relative overflow-hidden bg-muted", round ? "rounded-full" : "rounded-lg", className)}
      style={{ aspectRatio: String(a), ...style }}
      data-ckui="slot-image"
      data-empty={has ? undefined : ""}
    >
      {has ? (
        <RenditionImg
          {...img}
          key={key}
          outputs={outputs}
          density={density}
          alt={alt}
          className="absolute inset-0 size-full object-cover"
          onError={(e) => {
            setFailed(key);
            onError?.(e);
          }}
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

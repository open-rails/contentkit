import { ratio, type AspectRatio } from "../aspect.js";
import { Video01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { ComponentProps, ReactNode } from "react";
import { manifestAspect } from "../srcset.js";
import type { DensityRange } from "../rendition.js";
import { RenditionImg } from "./rendition-img.js";
import type { SlotManifest } from "../wire.gen.js";
import { UploadUiRoot } from "../scope.js";

export interface VideoPosterProps extends Omit<ComponentProps<"div">, "children"> {
  /** VideoImages.poster, or a listing's poster outputs as a SlotManifest. */
  poster?: SlotManifest | null;
  /** The box's "W:H"; default the cover's own (native) aspect, else "16:9". */
  aspect?: AspectRatio;
  /** Density range for picking the cover's rendition; default the provider's (2–3×). */
  density?: DensityRange;
  alt?: string;
  /** Shown without a poster; default a muted box with a video icon. */
  placeholder?: ReactNode;
  /** Overlays (duration, badges). */
  children?: ReactNode;
}

/**
 * A cover at its native aspect, uncropped, spanning its container's width, at
 * the rendition its rendered width × density needs. It never plays: use it for
 * videos the viewer cannot play (a locked post's teaser); playable ones
 * preview inline in VideoPlayer and MediaGallery.
 */
export function VideoPoster({ poster, aspect, density, alt = "", placeholder, className, style, children, ...div }: VideoPosterProps) {
  const has = !!poster?.outputs.some((o) => o.url);
  return (
    <UploadUiRoot
      {...div}
      className={cn("relative w-full overflow-hidden rounded-lg bg-muted", className)}
      style={{ aspectRatio: String(ratio(aspect ?? manifestAspect(poster, "16:9")) ?? 16 / 9), ...style }}
      data-ckui="video-poster"
    >
      {has ? (
        <RenditionImg outputs={poster!.outputs} density={density} alt={alt} loading="lazy" className="absolute inset-0 size-full object-contain" />
      ) : (
        (placeholder ?? (
          <div className="absolute inset-0 flex items-center justify-center text-muted-foreground/70">
            <HugeiconsIcon icon={Video01Icon} strokeWidth={1.5} className="size-10" />
          </div>
        ))
      )}
      {children}
    </UploadUiRoot>
  );
}

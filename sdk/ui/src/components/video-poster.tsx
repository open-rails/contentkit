import { ratio, type AspectRatio } from "../client/aspect.js";
import { Video01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import type { ComponentProps, ReactNode } from "react";
import { publicRenditions, type PublicPreset } from "../client/public.js";
import type { DensityRange } from "../client/rendition.js";
import { RenditionImg } from "./rendition-img.js";
import { ContentKitUiRoot } from "../scope.js";

export interface VideoPosterProps extends Omit<ComponentProps<"div">, "children"> {
  /** The poster's public preset (its aspect is the video's), or an image URL. */
  poster?: PublicPreset | string | null;
  /** The box's "W:H"; default the preset's aspect, else "16:9". */
  aspect?: AspectRatio;
  /** Density range for picking the poster's width; default the provider's (2–3×). */
  density?: DensityRange;
  alt?: string;
  /** Shown without a poster (or when it fails to load); default a muted box with a video icon. */
  fallback?: ReactNode;
  /** Overlays (duration, badges). */
  children?: ReactNode;
}

/**
 * A poster at its aspect, uncropped, spanning its container's width, at the
 * width its rendered width × density needs. It never plays: use it for
 * videos the viewer cannot play (a locked post); playable ones
 * preview inline in VideoPlayer and MediaGallery.
 */
export function VideoPoster({ poster, aspect, density, alt = "", fallback, className, style, children, ...div }: VideoPosterProps) {
  const preset = typeof poster === "string" ? null : poster;
  const outputs = publicRenditions(preset);
  const shape = aspect ?? (preset?.aspect || "16:9");
  const img = "absolute inset-0 size-full object-contain";
  const empty = fallback ?? (
    <div className="absolute inset-0 flex items-center justify-center text-muted-foreground/70">
      <HugeiconsIcon icon={Video01Icon} strokeWidth={1.5} className="size-10" />
    </div>
  );
  return (
    <ContentKitUiRoot
      {...div}
      className={cn("relative w-full overflow-hidden rounded-lg bg-muted", className)}
      style={{ aspectRatio: String(ratio(shape) ?? 16 / 9), ...style }}
      data-ckui="video-poster"
    >
      {typeof poster === "string" ? (
        <img src={poster} alt={alt} loading="lazy" decoding="async" className={img} />
      ) : outputs.length ? (
        <RenditionImg outputs={outputs} density={density} alt={alt} loading="lazy" className={img} fallback={empty} />
      ) : (
        empty
      )}
      {children}
    </ContentKitUiRoot>
  );
}

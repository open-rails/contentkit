import { Video01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useState, type ComponentProps, type ReactNode } from "react";
import { manifestAspect } from "../srcset.js";
import type { DensityRange } from "../rendition.js";
import { RenditionImg } from "./rendition-img.js";
import type { PreviewImage, SlotManifest } from "../wire.gen.js";
import { UploadUiRoot } from "../scope.js";

/** A hover preview's outputs: VideoImages.hover_preview, or URLs from a listing (Reader.HoverPreviewURLs). */
export interface HoverPreviewSource {
  mp4?: string | PreviewImage[];
  webp?: string | PreviewImage[];
}

const pick = (v: string | PreviewImage[] | undefined, width: number) => {
  if (typeof v === "string" || !v) return v || undefined;
  const sorted = [...v].sort((a, b) => a.w - b.w);
  return (sorted.find((o) => o.w >= width) ?? sorted.at(-1))?.url;
};

/** The user's reduced-motion preference, live. */
export function useReducedMotion(): boolean {
  const query = "(prefers-reduced-motion: reduce)";
  const [reduced, setReduced] = useState(() => typeof matchMedia === "function" && matchMedia(query).matches);
  useEffect(() => {
    if (typeof matchMedia !== "function") return;
    const m = matchMedia(query);
    const on = () => setReduced(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return reduced;
}

export interface HoverPreviewProps {
  preview?: HoverPreviewSource | null;
  /** Plays while true. */
  active: boolean;
  /** Rendered width in CSS pixels, to pick the output. Default 320. */
  width?: number;
  className?: string;
}

/**
 * The silent loop: a muted MP4 (smaller), falling back to the animated WebP.
 * Renders nothing when inactive, without outputs, or with reduced motion.
 */
export function HoverPreview({ preview, active, width = 320, className }: HoverPreviewProps) {
  const reduced = useReducedMotion();
  const density = typeof devicePixelRatio === "number" ? devicePixelRatio : 1;
  const mp4 = pick(preview?.mp4, width * density);
  const webp = pick(preview?.webp, width * density);
  const [failed, setFailed] = useState<string>();
  if (!active || reduced) return null;
  const cls = cn("pointer-events-none absolute inset-0 size-full object-cover", className);
  if (mp4 && failed !== mp4)
    return <video key={mp4} src={mp4} className={cls} muted loop playsInline autoPlay preload="auto" data-ckui="hover-preview" onError={() => setFailed(mp4)} />;
  if (webp && failed !== webp) return <img key={webp} src={webp} alt="" className={cls} data-ckui="hover-preview" onError={() => setFailed(webp)} />;
  return null;
}

export interface VideoPosterProps extends Omit<ComponentProps<"div">, "children"> {
  /** VideoImages.poster, or a listing's poster outputs as a SlotManifest. */
  poster?: SlotManifest | null;
  preview?: HoverPreviewSource | null;
  /** Controls playback (e.g. hover on a whole card); default hover or focus within this element. */
  playing?: boolean;
  /** Width / height of the box; default the poster's own (native) aspect, else the video's. */
  aspect?: number;
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
 * the rendition its rendered width × density needs; plays the hover preview
 * on hover or focus.
 */
export function VideoPoster({ poster, preview, playing, aspect, density, alt = "", placeholder, className, style, children, ...div }: VideoPosterProps) {
  const [hover, setHover] = useState(false);
  const [focus, setFocus] = useState(false);
  const has = !!poster?.outputs.some((o) => o.url);
  const active = playing ?? (hover || focus);
  return (
    <UploadUiRoot
      {...div}
      className={cn("relative w-full overflow-hidden rounded-lg bg-muted", className)}
      style={{ aspectRatio: String(aspect ?? manifestAspect(poster, 16 / 9)), ...style }}
      data-ckui="video-poster"
      data-playing={active && preview ? "" : undefined}
      onPointerEnter={(e) => {
        setHover(true);
        div.onPointerEnter?.(e);
      }}
      onPointerLeave={(e) => {
        setHover(false);
        div.onPointerLeave?.(e);
      }}
      onFocus={(e) => {
        setFocus(true);
        div.onFocus?.(e);
      }}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setFocus(false);
        div.onBlur?.(e);
      }}
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
      <HoverPreview preview={preview} active={active} />
      {children}
    </UploadUiRoot>
  );
}

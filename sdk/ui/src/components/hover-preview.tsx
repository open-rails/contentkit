import { useCallback, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { RefBody } from "../client/generated/wire.js";
import { useContentKitClient } from "../react/context.js";
import { useHlsPlayer, type HlsPlayerOptions } from "../react/gallery.js";
import { useInlinePreview } from "../react/inline-preview.js";

export interface HoverPreviewProps extends Pick<HlsPlayerOptions, "abr"> {
  /** The video item. */
  item: RefBody;
  /** Its HLS folder (a read's `hls` entry). Default "hls/". */
  dir?: string;
  /** Seconds; the preview starts 10% in. */
  duration?: number;
  /** Where the preview starts, seconds; overrides the 10% default. */
  start?: number;
  /** The viewer may play it (free, or entitled). Default true. */
  available?: boolean;
  /** What is hovered (fine pointers) or watched for visibility (touch); default the preview's parent, the card. */
  target?: HTMLElement | null;
  /** Previews on; default the provider's `inlinePreview`. */
  enabled?: boolean;
  className?: string;
  client?: ContentKitClient;
}

/**
 * A card's muted inline preview: the item's real HLS stream through the
 * client (its base and playlist auth) at the lowest rendition, fading in over
 * the card's thumbnail on hover, or while it is the most visible card on
 * touch screens. One plays page-wide; none with reduced motion or Save-Data.
 * Place it inside the card, which must be positioned.
 */
export function HoverPreview({ item, dir = "hls/", duration, start, available = true, target, enabled, abr, className, client: own }: HoverPreviewProps) {
  const { media } = useContentKitClient(own);
  const [el, setEl] = useState<HTMLVideoElement | null>(null);
  const player = useHlsPlayer({ src: `${media.hlsBase(item, dir)}master.m3u8`, xhrSetup: media.xhrSetup, abr, qualityKey: null });
  const d = duration ?? 0;
  const at = Math.max(0, start ?? (d > 1 ? Math.min(d * 0.1, d - 1) : 0));
  useInlinePreview({ setting: enabled, available, target: target ?? el?.parentElement ?? null, start: () => player.preview(at), stop: player.unload });
  const attach = player.ref;
  const ref = useCallback(
    (v: HTMLVideoElement | null) => {
      attach(v);
      setEl(v);
    },
    [attach],
  );
  return (
    <video
      ref={ref}
      muted
      playsInline
      preload="none"
      aria-hidden
      tabIndex={-1}
      className={className}
      style={{
        position: "absolute",
        inset: 0,
        width: "100%",
        height: "100%",
        objectFit: "cover",
        pointerEvents: "none",
        opacity: player.previewing && player.status === "playing" ? 1 : 0,
        transition: "opacity 200ms",
      }}
      data-ckui="hover-preview"
      data-playing={player.previewing && player.status === "playing" ? "" : undefined}
    />
  );
}

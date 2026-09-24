import { AlertCircleIcon, PlayIcon, Refresh01Icon, Settings01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useRef, useState, type CSSProperties } from "react";
import type { UploadUiAppearance } from "../appearance.js";
import { formatDuration } from "../gallery.js";
import { useHlsPlayer, type HlsPlayerOptions, type PlayerQuality } from "../gallery-react.js";
import { useMessages } from "../i18n/context.js";
import { UploadUiRoot } from "../scope.js";
import { RenditionImg } from "./rendition-img.js";
import type { EncodeProgress as Progress, SlotManifest } from "../wire.gen.js";
import { EncodeProgress } from "./encode-progress.js";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";

type XhrSetup = HlsPlayerOptions["xhrSetup"];

interface SpriteTile {
  href: string;
  x: number;
  y: number;
  w: number;
  h: number;
  width: number;
  height: number;
}

const tiles = new Map<string, Promise<SpriteTile | null>>();

function loadTile(vtt: string, setup: XhrSetup): Promise<SpriteTile | null> {
  let p = tiles.get(vtt);
  if (!p) {
    p = new Promise<string>((resolve) => {
      const xhr = new XMLHttpRequest();
      xhr.open("GET", vtt, true);
      xhr.onload = () => resolve(xhr.status === 200 ? xhr.responseText : "");
      xhr.onerror = () => resolve("");
      Promise.resolve(setup?.(xhr, vtt)).then(() => xhr.send(), () => resolve(""));
    }).then((text) => {
      const m = text.match(/^(\S+)#xywh=(\d+),(\d+),(\d+),(\d+)$/m);
      if (!m) return null;
      const href = new URL(m[1]!, new URL(vtt, globalThis.location?.href)).href;
      const [x, y, w, h] = m.slice(2).map(Number) as [number, number, number, number];
      return new Promise<SpriteTile | null>((resolve) => {
        const img = new Image();
        img.onload = () => resolve({ href, x, y, w, h, width: img.naturalWidth, height: img.naturalHeight });
        img.onerror = () => resolve(null);
        img.src = href;
      });
    });
    tiles.set(vtt, p);
    void p.then((t) => t || tiles.delete(vtt));
  }
  return p;
}

/** The first frame of a video's seek sprite (sprite.vtt), fitted like object-fit. */
export function SpriteFrame({ vtt, xhrSetup, fit = "contain", className }: { vtt: string; xhrSetup?: XhrSetup; fit?: "contain" | "cover"; className?: string }) {
  const [tile, setTile] = useState<SpriteTile | null>(null);
  useEffect(() => {
    let live = true;
    void loadTile(vtt, xhrSetup).then((t) => live && setTile(t));
    return () => {
      live = false;
    };
    // xhrSetup is read once per sprite
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [vtt]);
  if (!tile) return null;
  return (
    <svg
      aria-hidden
      className={cn("pointer-events-none absolute inset-0 size-full", className)}
      viewBox={`${tile.x} ${tile.y} ${tile.w} ${tile.h}`}
      preserveAspectRatio={fit === "cover" ? "xMidYMid slice" : "xMidYMid meet"}
      data-ckui="sprite-frame"
    >
      <image href={tile.href} width={tile.width} height={tile.height} />
    </svg>
  );
}

export interface VideoPlayerProps extends Omit<HlsPlayerOptions, "src"> {
  /** The folder of the video's HLS (…/hls/{file}/): master.m3u8 and sprite.vtt resolve under it. */
  base?: string | null;
  /** Shown until playback starts; default the first sprite frame. */
  poster?: string | SlotManifest | null;
  /** The source's size (read API w/h): the box is reserved before anything loads. */
  width?: number;
  height?: number;
  duration?: number;
  /** A pending encode (read API `progress`): shows its progress instead of a player. */
  pending?: boolean;
  progress?: Progress | null;
  /** Why the video cannot be encoded (read API `failed`, editors only). */
  failed?: string;
  /**
   * `frame` spans the width at the video's aspect, capped at `maxHeight` and
   * letterboxed; `fill` fills a sized parent (a carousel slide).
   */
  layout?: "frame" | "fill";
  maxHeight?: string;
  label?: string;
  className?: string;
  style?: CSSProperties;
  appearance?: UploadUiAppearance;
}

/**
 * An HLS player that never spins forever: poster and play control, native
 * controls once playing, and a specific error with Retry and a support code.
 */
export function VideoPlayer({
  base,
  poster,
  width,
  height,
  duration,
  pending,
  progress,
  failed,
  layout = "frame",
  maxHeight = "80svh",
  label,
  className,
  style,
  appearance,
  ...options
}: VideoPlayerProps) {
  const { t } = useMessages();
  const playable = !!base && !pending && !failed;
  const player = useHlsPlayer({ ...options, src: playable ? `${base}master.m3u8` : null });
  const { status, error, started } = player;
  const aspect = width && height ? width / height : 16 / 9;
  const frame: CSSProperties = layout === "frame" ? { aspectRatio: String(aspect), maxHeight } : {};
  const posterOutputs = typeof poster === "string" ? [] : (poster?.outputs ?? []).filter((o) => o.url);
  const busy = !error && (status === "loading" || status === "buffering");
  return (
    <UploadUiRoot
      appearance={appearance}
      className={cn("relative w-full overflow-hidden bg-black text-white", layout === "fill" ? "size-full" : "rounded-lg", className)}
      style={{ ...frame, ...style }}
      data-ckui="video-player"
      data-status={failed ? "failed" : pending ? "pending" : status}
      aria-label={label}
      role={label ? "group" : undefined}
    >
      {failed ? (
        <Panel icon>{t("player.failed")}</Panel>
      ) : pending || !base ? (
        <div className="absolute inset-0 flex items-center justify-center bg-muted p-6 text-foreground">
          <EncodeProgress progress={progress} className="max-w-xs" />
        </div>
      ) : (
        <>
          <video
            ref={player.ref}
            className="absolute inset-0 size-full object-contain"
            playsInline
            preload="none"
            controls={started && !error}
            aria-label={label}
          />
          {!started && !error && (
            <>
              {typeof poster === "string" ? (
                <img alt="" src={poster} decoding="async" className="pointer-events-none absolute inset-0 size-full object-contain" />
              ) : posterOutputs.length ? (
                <RenditionImg outputs={posterOutputs} className="pointer-events-none absolute inset-0 size-full object-contain" />
              ) : (
                <SpriteFrame vtt={`${base}sprite.vtt`} xhrSetup={options.xhrSetup} />
              )}
              <button
                type="button"
                className="absolute inset-0 flex items-center justify-center outline-none focus-visible:[&>span]:ring-4 focus-visible:[&>span]:ring-white/60"
                onClick={player.play}
                aria-label={t("player.play")}
                disabled={busy}
              >
                <span className="flex size-16 items-center justify-center rounded-full bg-black/55 shadow-lg backdrop-blur-sm transition-transform motion-safe:hover:scale-105">
                  {busy ? <Spinner /> : <HugeiconsIcon icon={PlayIcon} className="ml-1 size-7 fill-current" strokeWidth={1.5} />}
                </span>
              </button>
              {duration ? (
                <span className="pointer-events-none absolute right-2 bottom-2 rounded bg-black/65 px-1.5 py-0.5 text-xs font-medium tabular-nums">
                  {formatDuration(duration)}
                </span>
              ) : null}
            </>
          )}
          {started && !error && player.quality.levels.length > 1 && <QualityMenu quality={player.quality} />}
          {started && busy && (
            <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
              <span className="flex size-14 items-center justify-center rounded-full bg-black/45">
                <Spinner />
              </span>
            </div>
          )}
          <span className="sr-only" aria-live="polite">
            {busy ? t("player.loading") : ""}
          </span>
          {error && (
            <Panel icon code={error.code} onRetry={player.retry}>
              {t(`player.errors.${error.kind}`)}
            </Panel>
          )}
        </>
      )}
    </UploadUiRoot>
  );
}

/** "2160p 4K", "1080p HD", "720p": a rung by its short side. */
export function qualityLabel(height: number): string {
  return `${height}p${height >= 2160 ? " 4K" : height === 1080 ? " HD" : ""}`;
}

function QualityMenu({ quality }: { quality: PlayerQuality }) {
  const { t } = useMessages();
  const trigger = useRef<HTMLButtonElement>(null);
  const [container, setContainer] = useState<HTMLElement | undefined>();
  const { levels, selected, current, select } = quality;
  const playing = levels.find((l) => l.index === current);
  return (
    <DropdownMenu
      onOpenChange={(open) => {
        // In an element-fullscreen player the menu must render inside the fullscreen element.
        const fs = typeof document === "undefined" ? null : document.fullscreenElement;
        if (open) setContainer(fs instanceof HTMLElement && trigger.current && fs.contains(trigger.current) ? fs : undefined);
      }}
    >
      <DropdownMenuTrigger
        ref={trigger}
        data-ckui="quality"
        data-ckui-noswipe=""
        aria-label={t("player.quality")}
        title={t("player.quality")}
        className="absolute top-2 right-2 flex size-9 items-center justify-center rounded-full bg-black/55 text-white outline-none backdrop-blur-sm hover:bg-black/70 focus-visible:ring-2 focus-visible:ring-white/70"
      >
        <HugeiconsIcon icon={Settings01Icon} className="size-5" strokeWidth={1.75} />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" container={container} className="min-w-40" data-ckui="quality-menu">
        <DropdownMenuGroup>
          <DropdownMenuLabel>{t("player.quality")}</DropdownMenuLabel>
          <DropdownMenuRadioGroup value={String(selected)} onValueChange={(v) => select(Number(v))}>
            {[...levels].reverse().map((l) => (
              <DropdownMenuRadioItem key={l.index} value={String(l.index)} closeOnClick>
                {qualityLabel(l.height)}
              </DropdownMenuRadioItem>
            ))}
            <DropdownMenuRadioItem value="-1" closeOnClick>
              {playing && selected === -1 ? t("player.autoCurrent", { quality: qualityLabel(playing.height).split(" ")[0]! }) : t("player.auto")}
            </DropdownMenuRadioItem>
          </DropdownMenuRadioGroup>
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function Spinner() {
  return <span aria-hidden className="size-6 rounded-full border-2 border-white/30 border-t-white motion-safe:animate-spin" />;
}

function Panel({ children, icon, code, onRetry }: { children: string; icon?: boolean; code?: string; onRetry?: () => void }) {
  const { t } = useMessages();
  return (
    <div role="alert" data-ckui="player-error" data-ckui-noswipe="" className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black/80 p-6 text-center">
      {icon && <HugeiconsIcon icon={AlertCircleIcon} className="size-7 text-white/80" strokeWidth={1.75} />}
      <p className="max-w-sm text-sm font-medium text-balance">{children}</p>
      {onRetry && (
        <Button size="sm" variant="secondary" onClick={onRetry}>
          <HugeiconsIcon icon={Refresh01Icon} data-icon="inline-start" />
          {t("player.retry")}
        </Button>
      )}
      {code && <p className="font-mono text-[11px] text-white/55">{t("player.code", { code })}</p>}
    </div>
  );
}

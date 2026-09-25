import { Dialog as DialogPrimitive } from "@base-ui/react/dialog";
import {
  AlertCircleIcon,
  ArrowLeft01Icon,
  ArrowRight01Icon,
  Cancel01Icon,
  CarouselHorizontalIcon,
  Download01Icon,
  GridViewIcon,
  MusicNote01Icon,
  PlayIcon,
  SquareLock02Icon,
} from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useMemo, useState, type ReactNode } from "react";
import type { UploadUiAppearance } from "../appearance.js";
import { audioDownloadKey, formatDuration, galleryItems, stageAspect, type GalleryItem, type GalleryLockedItem, type GalleryMediaItem } from "../gallery.js";
import { useCarousel, useGalleryView, useHlsPlayer, type GalleryViewOptions, type HlsPlayerOptions } from "../gallery-react.js";
import { useInlinePreview } from "../inline-preview.js";
import { useMessages } from "../i18n/context.js";
import { UploadUiRoot, useScopeProps } from "../scope.js";
import { RenditionImg } from "./rendition-img.js";
import type { FileInfo, ReadResult, VideoImages } from "../wire.gen.js";
import { previewStartAt, SpriteFrame, VideoPlayer } from "./video-player.js";
import { Button } from "#ckui/ui/button";
import { ToggleGroup, ToggleGroupItem } from "#ckui/ui/toggle-group";

export interface MediaGalleryProps extends GalleryViewOptions, Pick<HlsPlayerOptions, "xhrSetup" | "refresh" | "abr"> {
  /**
   * The read API result: files in manifest order with this viewer's access.
   * Audio files play from their `audio` variant (AUDIO_VARIANT), so include it
   * in the read's variants; full access adds their download.
   */
  read: ReadResult | null | undefined;
  /** A video file's HLS folder (e.g. `/media/post/1/hls/{name}/`). */
  hlsBase?: (file: FileInfo) => string;
  /** The item's poster; drawn for the video it was cut from (or the first video), whose preview starts at its frame. */
  videoImages?: VideoImages | null;
  /** Playable videos preview muted inline (hover, or in view on touch); default the provider's. */
  inlinePreview?: boolean;
  /** The host's unlock call to action, drawn over the locked item. */
  renderLocked?: (locked: { count: number; videos: number }) => ReactNode;
  /** Below the carousel, for its current item (e.g. downloads). */
  renderDetails?: (item: GalleryItem) => ReactNode;
  /** `sizes` for carousel images. Default "(min-width: 768px) 720px, 100vw". */
  sizes?: string;
  /** Tallest the carousel gets; taller media letterboxes. Default none: every slide at its native aspect, full width. */
  maxHeight?: string;
  label?: string;
  className?: string;
  appearance?: UploadUiAppearance;
}

interface Ctx extends MediaGalleryProps {
  items: GalleryItem[];
}

// The item's poster belongs to the video it was cut from, whose preview starts
// at its frame; an uploaded poster (no file) to the first video.
function videoArt(ctx: Ctx, f: FileInfo): { poster?: VideoImages["poster"]; start: number } {
  const v = ctx.videoImages;
  const first = ctx.items.find((i): i is GalleryMediaItem => i.kind === "video")?.file.name;
  const file = v?.poster.file ?? v?.poster.selection?.file;
  const mine = !!v && (file || first) === f.name;
  const time = mine && file ? v?.poster.time : undefined;
  return { poster: mine && v.poster.outputs.length ? v.poster : undefined, start: previewStartAt(time, f.duration) };
}

/**
 * A post's images and videos as a swipeable carousel or a tile grid that opens
 * a lightbox, with a view toggle. One item renders alone.
 */
export function MediaGallery(props: MediaGalleryProps) {
  const { read, label, className, appearance, view: given, defaultView, onViewChange, storageKey } = props;
  const { t } = useMessages();
  const items = useMemo(() => galleryItems(read), [read]);
  const [view, setView] = useGalleryView({ view: given, defaultView, onViewChange, storageKey });
  const [index, setIndex] = useState(0);
  const [lightbox, setLightbox] = useState<number | null>(null);
  if (items.length === 0) return null;
  const ctx: Ctx = { ...props, items };
  const multi = items.length > 1;
  const current = Math.min(index, items.length - 1);
  const shown = multi ? view : "carousel";
  return (
    <UploadUiRoot appearance={appearance} className={cn("@container grid gap-2", className)} data-ckui="media-gallery" data-view={shown} role="region" aria-label={label ?? t("gallery.label")}>
      {multi && (
        <div className="flex items-center justify-between gap-2" data-ckui="gallery-header">
          <span className="text-sm text-muted-foreground tabular-nums" data-ckui="gallery-count">
            {shown === "carousel" ? t("gallery.counter", { current: current + 1, total: items.length }) : null}
          </span>
          <ToggleGroup aria-label={t("gallery.view")} value={[shown]} onValueChange={(v) => v[0] && setView(v[0] as typeof view)}>
            <ToggleGroupItem value="carousel" aria-label={t("gallery.carousel")} title={t("gallery.carousel")}>
              <HugeiconsIcon icon={CarouselHorizontalIcon} strokeWidth={2} />
            </ToggleGroupItem>
            <ToggleGroupItem value="grid" aria-label={t("gallery.grid")} title={t("gallery.grid")}>
              <HugeiconsIcon icon={GridViewIcon} strokeWidth={2} />
            </ToggleGroupItem>
          </ToggleGroup>
        </div>
      )}
      {shown === "carousel" ? (
        <>
          <Carousel ctx={ctx} index={current} onIndex={setIndex} />
          {props.renderDetails?.(items[current]!)}
        </>
      ) : (
        <Grid ctx={ctx} onOpen={setLightbox} />
      )}
      <Lightbox ctx={ctx} index={lightbox} onIndex={setLightbox} />
    </UploadUiRoot>
  );
}

function Carousel({ ctx, index: given, onIndex, lightbox }: { ctx: Ctx; index: number; onIndex: (i: number) => void; lightbox?: boolean }) {
  const { t } = useMessages();
  const { items } = ctx;
  const c = useCarousel({ count: items.length, index: given, onIndexChange: onIndex });
  const { index } = c;
  const multi = items.length > 1;
  const stage = lightbox ? {} : { aspectRatio: String(stageAspect(items, index)), maxHeight: ctx.maxHeight };
  return (
    <div
      className={cn("group/carousel relative outline-none", lightbox ? "size-full" : "grid gap-2")}
      role={multi ? "group" : undefined}
      aria-roledescription={multi ? "carousel" : undefined}
      aria-label={multi ? (ctx.label ?? t("gallery.label")) : undefined}
      tabIndex={multi && !lightbox ? 0 : undefined}
      onKeyDown={multi && !lightbox ? c.onKeyDown : undefined}
      data-ckui="carousel"
    >
      <div
        className={cn(
          "relative w-full touch-pan-y overflow-hidden select-none",
          lightbox ? "h-full" : "rounded-lg bg-muted transition-[aspect-ratio] duration-300 ease-out motion-reduce:transition-none",
        )}
        style={stage}
        data-ckui="stage"
        {...(multi ? c.swipe : {})}
      >
        <div
          className={cn("flex h-full", c.dragging ? "transition-none" : "transition-transform duration-300 ease-out motion-reduce:transition-none")}
          style={{ transform: `translateX(calc(${-index * 100}% + ${c.offset}px))` }}
        >
          {items.map((item, i) => {
            const near = Math.abs(i - index) <= 1;
            return (
              <div
                key={item.key}
                className="relative h-full w-full shrink-0 overflow-hidden"
                role={multi ? "group" : undefined}
                aria-roledescription={multi ? "slide" : undefined}
                aria-label={multi ? t("gallery.slide", { current: i + 1, total: items.length }) : undefined}
                aria-hidden={i !== index || undefined}
                inert={i !== index}
                data-ckui="slide"
                data-current={i === index ? "" : undefined}
              >
                {near && <Slide ctx={ctx} item={item} position={i} active={i === index} lightbox={lightbox} />}
              </div>
            );
          })}
        </div>
        {multi && (
          <>
            <NavButton side="prev" hidden={index === 0} last={index === 1} onClick={c.prev} label={t("gallery.previous")} lightbox={lightbox} />
            <NavButton side="next" hidden={index === items.length - 1} last={index === items.length - 2} onClick={c.next} label={t("gallery.next")} lightbox={lightbox} />
          </>
        )}
      </div>
      {multi && !lightbox && items.length <= 12 && (
        <div className="flex justify-center gap-0.5" data-ckui="dots">
          {items.map((item, i) => (
            <button
              key={item.key}
              type="button"
              className="group/dot flex size-4 items-center justify-center rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring"
              aria-label={t("gallery.goTo", { index: i + 1 })}
              aria-current={i === index || undefined}
              onClick={() => c.go(i)}
            >
              <span className={cn("size-1.5 rounded-full transition-colors", i === index ? "bg-foreground" : "bg-foreground/25 group-hover/dot:bg-foreground/50")} />
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// A strip down the stage's side, clear of a video's control bar: the stage's
// height follows each slide's aspect, so a centred-only target would move
// between quick clicks. A swipe may start on it. The click that reaches an end
// removes the button, so focus moves to the carousel and the arrow keys keep working.
function NavButton({ side, hidden, last, onClick, label, lightbox }: { side: "prev" | "next"; hidden: boolean; last: boolean; onClick: () => void; label: string; lightbox?: boolean }) {
  if (hidden) return null;
  return (
    <button
      type="button"
      aria-label={label}
      data-ckui="gallery-nav"
      data-side={side}
      onClick={(e) => {
        const root = e.currentTarget.closest<HTMLElement>("[data-ckui=lightbox], [data-ckui=carousel][tabindex]");
        onClick();
        if (last && document.activeElement === e.currentTarget) root?.focus({ preventScroll: true });
      }}
      className={cn(
        "group/nav absolute top-0 bottom-14 z-10 flex items-center outline-none",
        lightbox ? "w-16 sm:w-20" : "w-12",
        side === "prev" ? (lightbox ? "left-0 justify-start pl-3 sm:pl-5" : "left-0 justify-start pl-2") : lightbox ? "right-0 justify-end pr-3 sm:pr-5" : "right-0 justify-end pr-2",
      )}
    >
      <span
        className={cn(
          "flex items-center justify-center rounded-full backdrop-blur-sm transition-colors group-focus-visible/nav:ring-3 group-focus-visible/nav:ring-ring/60",
          lightbox ? "size-10 bg-white/10 text-white group-hover/nav:bg-white/20" : "size-8 bg-white/85 text-zinc-900 shadow-md group-hover/nav:bg-white",
        )}
      >
        <HugeiconsIcon icon={side === "prev" ? ArrowLeft01Icon : ArrowRight01Icon} strokeWidth={2} className="size-4" />
      </span>
    </button>
  );
}

function Slide({ ctx, item, position, active, lightbox }: { ctx: Ctx; item: GalleryItem; position: number; active: boolean; lightbox?: boolean }) {
  const { t } = useMessages();
  if (item.kind === "locked") return <Locked ctx={ctx} item={item} />;
  const f = item.file;
  if (item.kind === "audio") return <AudioSlide ctx={ctx} file={f} position={position} />;
  if (item.kind === "image") {
    if (!f.url) return f.failed ? <ImageFailed file={f} /> : <Processing>{t("gallery.processingImage")}</Processing>;
    return (
      <img
        src={f.url}
        alt={t("gallery.image", { index: position + 1 })}
        width={f.w}
        height={f.h}
        sizes={lightbox ? "100vw" : (ctx.sizes ?? "(min-width: 768px) 720px, 100vw")}
        loading={active ? "eager" : "lazy"}
        decoding="async"
        draggable={false}
        className="absolute inset-0 size-full object-contain"
      />
    );
  }
  const { poster, start } = videoArt(ctx, f);
  return (
    <VideoPlayer
      inlinePreview={lightbox ? false : ctx.inlinePreview}
      previewStart={start}
      layout="fill"
      base={f.hls && f.name && ctx.hlsBase ? ctx.hlsBase(f) : null}
      pending={!f.hls && !f.failed}
      progress={f.progress}
      failed={f.failed}
      poster={poster}
      width={f.w}
      height={f.h}
      duration={f.duration}
      active={active}
      xhrSetup={ctx.xhrSetup}
      refresh={ctx.refresh}
      abr={ctx.abr}
      label={t("gallery.video", { index: position + 1 })}
      className={lightbox ? "bg-transparent" : undefined}
    />
  );
}

// An audio file: a native player over its M4A variant, its duration and,
// with full access, its download.
function AudioSlide({ ctx, file: f, position }: { ctx: Ctx; file: FileInfo; position: number }) {
  const { t } = useMessages();
  const download = f.name ? ctx.read?.downloads?.find((d) => d.key === audioDownloadKey(f.name!)) : undefined;
  const label = t("gallery.audio", { index: position + 1 });
  return (
    <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-muted p-4" data-ckui="audio">
      <div className="flex min-w-0 items-center gap-2 text-sm text-muted-foreground">
        <HugeiconsIcon icon={MusicNote01Icon} className="size-5 shrink-0" strokeWidth={1.75} />
        {f.name && <span className="truncate">{f.name}</span>}
        {f.duration ? <span className="tabular-nums">{formatDuration(f.duration)}</span> : null}
      </div>
      {f.failed ? (
        <p className="text-sm text-destructive" role="alert">
          {t("gallery.failedAudio")}
        </p>
      ) : f.url ? (
        <audio src={f.url} controls preload="metadata" aria-label={label} className="w-full max-w-md" data-ckui-noswipe="" />
      ) : (
        <p className="text-sm text-muted-foreground">{t("gallery.processingAudio")}</p>
      )}
      {download && (
        <a href={download.url} download={download.name} className="inline-flex items-center gap-1.5 text-sm underline-offset-4 hover:underline" data-ckui-noswipe="">
          <HugeiconsIcon icon={Download01Icon} className="size-4" strokeWidth={2} />
          {t("gallery.download")}
        </a>
      )}
    </div>
  );
}

// Why an image cannot be shown (editors only: the read API's failed fields).
function ImageFailed({ file }: { file: FileInfo }) {
  const { t, error } = useMessages();
  const text = file.failed_code ? error({ code: file.failed_code, message: file.failed, details: file.failed_details, refusal: true }) : t("gallery.failedImage");
  return (
    <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-muted p-4 text-center text-sm text-destructive" role="alert" data-ckui="image-failed">
      <HugeiconsIcon icon={AlertCircleIcon} className="size-6" />
      <p className="max-w-sm text-balance">{text}</p>
    </div>
  );
}

function Processing({ children }: { children: string }) {
  return (
    <div className="absolute inset-0 flex items-center justify-center gap-2 bg-muted text-sm text-muted-foreground">
      <span aria-hidden className="size-4 rounded-full border-2 border-current/30 border-t-current motion-safe:animate-spin" />
      {children}
    </div>
  );
}

function lockedText(t: ReturnType<typeof useMessages>["t"], count: number) {
  return count === 1 ? t("gallery.lockedOne") : t("gallery.lockedMany", { count });
}

function Locked({ ctx, item, tile }: { ctx: Ctx; item: GalleryLockedItem; tile?: boolean }) {
  const { t } = useMessages();
  return (
    <div className="absolute inset-0 overflow-hidden bg-muted" data-ckui="locked">
      {item.teaser?.url && <img src={item.teaser.url} alt="" draggable={false} className="absolute inset-0 size-full scale-105 object-cover" />}
      <div className={cn("absolute inset-0 flex flex-col items-center justify-center gap-3 p-4 text-center", item.teaser?.url ? "bg-black/45 text-white" : "text-foreground")}>
        <HugeiconsIcon icon={SquareLock02Icon} className={tile ? "size-6" : "size-8"} strokeWidth={1.75} />
        {tile ? (
          <span className="text-lg font-semibold tabular-nums">+{item.count}</span>
        ) : (
          <>
            <p className="text-sm font-medium text-balance">{lockedText(t, item.count)}</p>
            {ctx.renderLocked && <div data-ckui-noswipe="">{ctx.renderLocked({ count: item.count, videos: item.videos })}</div>}
          </>
        )}
      </div>
    </div>
  );
}

function Grid({ ctx, onOpen }: { ctx: Ctx; onOpen: (i: number) => void }) {
  const { t } = useMessages();
  const n = ctx.items.length;
  return (
    <ul className={cn("grid gap-1", n === 2 || n === 4 ? "grid-cols-2" : "grid-cols-2 @md:grid-cols-3")} data-ckui="grid">
      {ctx.items.map((item, i) => (
        <li key={item.key}>
          <Tile ctx={ctx} item={item} label={t("gallery.open", { index: i + 1, total: n })} onOpen={() => onOpen(i)} />
        </li>
      ))}
    </ul>
  );
}

function Tile({ ctx, item, label, onOpen }: { ctx: Ctx; item: GalleryItem; label: string; onOpen: () => void }) {
  const [el, setEl] = useState<HTMLButtonElement | null>(null);
  let body: ReactNode;
  if (item.kind === "locked") body = <Locked ctx={ctx} item={item} tile />;
  else if (item.kind === "audio")
    body = (
      <span className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-muted text-muted-foreground">
        <HugeiconsIcon icon={item.file.failed ? AlertCircleIcon : MusicNote01Icon} className={cn("size-7", item.file.failed && "text-destructive")} strokeWidth={1.75} />
        {item.file.duration ? <span className="text-[11px] font-medium tabular-nums">{formatDuration(item.file.duration)}</span> : null}
      </span>
    );
  else if (item.kind === "image")
    body = item.file.url ? (
      <img src={item.file.url} alt="" loading="lazy" decoding="async" draggable={false} className="absolute inset-0 size-full object-cover transition-transform duration-300 motion-safe:group-hover:scale-[1.03]" />
    ) : item.file.failed ? (
      <span className="absolute inset-0 flex items-center justify-center bg-muted text-destructive">
        <HugeiconsIcon icon={AlertCircleIcon} className="size-6" />
      </span>
    ) : (
      <Processing>{""}</Processing>
    );
  else {
    const f = item.file;
    const { poster, start } = videoArt(ctx, f);
    const covers = (poster?.outputs ?? []).filter((o) => o.url);
    const base = f.hls && f.name && ctx.hlsBase ? ctx.hlsBase(f) : null;
    body = (
      <>
        {covers.length ? (
          <RenditionImg outputs={covers} loading="lazy" className="absolute inset-0 size-full object-cover" />
        ) : base ? (
          <SpriteFrame vtt={`${base}sprite.vtt`} xhrSetup={ctx.xhrSetup} fit="cover" />
        ) : null}
        {base && <TilePreview ctx={ctx} base={base} start={start} target={el} />}
        {f.failed ? (
          <span className="absolute inset-0 flex items-center justify-center bg-muted text-destructive">
            <HugeiconsIcon icon={AlertCircleIcon} className="size-6" />
          </span>
        ) : !f.hls ? (
          <Processing>{""}</Processing>
        ) : (
          <span className="pointer-events-none absolute top-1/2 left-1/2 flex size-10 -translate-1/2 items-center justify-center rounded-full bg-black/55 text-white backdrop-blur-sm transition-opacity group-hover:opacity-0">
            <HugeiconsIcon icon={PlayIcon} className="ml-0.5 size-5 fill-current" strokeWidth={1.5} />
          </span>
        )}
        {f.duration ? (
          <span className="pointer-events-none absolute right-1.5 bottom-1.5 rounded bg-black/65 px-1.5 py-0.5 text-[11px] font-medium text-white tabular-nums">
            {formatDuration(f.duration)}
          </span>
        ) : null}
      </>
    );
  }
  return (
    <button
      type="button"
      aria-label={label}
      onClick={onOpen}
      ref={setEl}
      className="group relative block aspect-square w-full overflow-hidden rounded-md bg-muted outline-none focus-visible:ring-3 focus-visible:ring-ring/60"
      data-ckui="tile"
      data-kind={item.kind}
    >
      {body}
    </button>
  );
}

// A grid tile's inline preview; the tile opens the lightbox for real playback.
function TilePreview({ ctx, base, start, target }: { ctx: Ctx; base: string; start: number; target: HTMLElement | null }) {
  const player = useHlsPlayer({ src: `${base}master.m3u8`, xhrSetup: ctx.xhrSetup, refresh: ctx.refresh, abr: ctx.abr, qualityKey: null });
  useInlinePreview({ setting: ctx.inlinePreview, available: true, target, start: () => player.preview(start), stop: player.unload });
  return (
    <video
      ref={player.ref}
      muted
      playsInline
      preload="none"
      aria-hidden
      tabIndex={-1}
      className={cn(
        "pointer-events-none absolute inset-0 size-full object-cover opacity-0 transition-opacity duration-300 motion-reduce:transition-none",
        player.previewing && player.status === "playing" && "opacity-100",
      )}
      data-ckui="tile-preview"
    />
  );
}

function Lightbox({ ctx, index, onIndex }: { ctx: Ctx; index: number | null; onIndex: (i: number | null) => void }) {
  const { t } = useMessages();
  const scope = useScopeProps(ctx.appearance);
  const [shown, setShown] = useState(index ?? 0);
  const [prev, setPrev] = useState(index);
  if (index !== prev) {
    setPrev(index);
    if (index !== null) setShown(index);
  }
  const open = index !== null;
  const c = useCarousel({ count: ctx.items.length, index: shown, onIndexChange: setShown });
  return (
    <DialogPrimitive.Root open={open} onOpenChange={(o) => !o && onIndex(null)}>
      <DialogPrimitive.Portal {...scope} data-ckui-theme="dark">
        <DialogPrimitive.Backdrop className="fixed inset-0 z-50 bg-black duration-150 data-open:animate-in data-open:fade-in-0 data-closed:animate-out data-closed:fade-out-0 motion-reduce:animate-none" />
        <DialogPrimitive.Popup
          className="fixed inset-0 z-50 flex flex-col bg-black text-white outline-none duration-150 data-open:animate-in data-open:fade-in-0 data-closed:animate-out data-closed:fade-out-0 motion-reduce:animate-none"
          data-ckui="lightbox"
          onKeyDown={c.onKeyDown}
        >
          <DialogPrimitive.Title className="sr-only">{ctx.label ?? t("gallery.lightbox")}</DialogPrimitive.Title>
          <div className="flex h-14 shrink-0 items-center justify-between px-4">
            <span className="text-sm text-white/80 tabular-nums">{ctx.items.length > 1 ? t("gallery.counter", { current: shown + 1, total: ctx.items.length }) : null}</span>
            <DialogPrimitive.Close render={<Button variant="ghost" size="icon" className="rounded-full text-white hover:bg-white/10 hover:text-white" />}>
              <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} />
              <span className="sr-only">{t("common.close")}</span>
            </DialogPrimitive.Close>
          </div>
          <div className="min-h-0 flex-1 pb-4 sm:px-4 sm:pb-6">
            {open && <Carousel ctx={ctx} index={shown} onIndex={setShown} lightbox />}
          </div>
        </DialogPrimitive.Popup>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

import { Dialog as DialogPrimitive } from "@base-ui/react/dialog";
import {
  AlertCircleIcon,
  ArrowLeft01Icon,
  ArrowRight01Icon,
  Cancel01Icon,
  CarouselHorizontalIcon,
  GridViewIcon,
  PlayIcon,
  SquareLock02Icon,
} from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useMemo, useState, type ReactNode } from "react";
import type { UploadUiAppearance } from "../appearance.js";
import { formatDuration, galleryItems, stageAspect, type GalleryItem, type GalleryLockedItem, type GalleryMediaItem } from "../gallery.js";
import { useCarousel, useGalleryView, type GalleryViewOptions, type HlsPlayerOptions } from "../gallery-react.js";
import { useMessages } from "../i18n/context.js";
import { UploadUiRoot, useScopeProps } from "../scope.js";
import { slotSources } from "../srcset.js";
import type { FileInfo, ReadResult, VideoImages } from "../wire.gen.js";
import { HoverPreview } from "./video-poster.js";
import { SpriteFrame, VideoPlayer } from "./video-player.js";
import { Button } from "#ckui/ui/button";
import { ToggleGroup, ToggleGroupItem } from "#ckui/ui/toggle-group";

export interface MediaGalleryProps extends GalleryViewOptions, Pick<HlsPlayerOptions, "xhrSetup" | "refresh" | "abr"> {
  /** The read API result: files in manifest order with this viewer's access. */
  read: ReadResult | null | undefined;
  /** A video file's HLS folder (e.g. `/media/post/1/hls/{name}/`). */
  hlsBase?: (file: FileInfo) => string;
  /** The item's poster and hover preview; drawn for the video they were cut from (or the only video). */
  videoImages?: VideoImages | null;
  /** The host's unlock call to action, drawn over the locked item. */
  renderLocked?: (locked: { count: number; videos: number }) => ReactNode;
  /** Below the carousel, for its current item (e.g. downloads). */
  renderDetails?: (item: GalleryItem) => ReactNode;
  /** `sizes` for carousel images. Default "(min-width: 768px) 720px, 100vw". */
  sizes?: string;
  /** Tallest the carousel gets; taller media letterboxes. Default "80svh". */
  maxHeight?: string;
  label?: string;
  className?: string;
  appearance?: UploadUiAppearance;
}

interface Ctx extends MediaGalleryProps {
  items: GalleryItem[];
}

function videoArt(ctx: Ctx, f: FileInfo) {
  const v = ctx.videoImages;
  if (!v) return {};
  const chosen = v.poster.selection?.file ?? v.hover_preview.selection?.file ?? v.video?.file;
  const only = ctx.items.filter((i) => i.kind === "video").length === 1;
  if (chosen ? chosen !== f.name : !only) return {};
  return { poster: v.poster.outputs.length ? v.poster : undefined, preview: v.hover_preview };
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
        <div className="flex items-center justify-between gap-2">
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
  const stage = lightbox ? {} : { aspectRatio: String(stageAspect(items)), maxHeight: ctx.maxHeight ?? "80svh" };
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
        className={cn("relative w-full touch-pan-y overflow-hidden select-none", lightbox ? "h-full" : "rounded-lg bg-muted")}
        style={stage}
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
            <NavButton side="prev" hidden={index === 0} onClick={c.prev} label={t("gallery.previous")} lightbox={lightbox} />
            <NavButton side="next" hidden={index === items.length - 1} onClick={c.next} label={t("gallery.next")} lightbox={lightbox} />
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

function NavButton({ side, hidden, onClick, label, lightbox }: { side: "prev" | "next"; hidden: boolean; onClick: () => void; label: string; lightbox?: boolean }) {
  if (hidden) return null;
  return (
    <Button
      variant="secondary"
      size={lightbox ? "icon-lg" : "icon-sm"}
      aria-label={label}
      data-ckui-noswipe=""
      onClick={onClick}
      className={cn(
        "absolute top-1/2 z-10 -translate-y-1/2 rounded-full backdrop-blur-sm",
        lightbox ? "bg-white/10 text-white hover:bg-white/20 hover:text-white" : "bg-white/85 text-zinc-900 shadow-md hover:bg-white hover:text-zinc-900 dark:bg-white/85 dark:hover:bg-white",
        side === "prev" ? (lightbox ? "left-3 sm:left-5" : "left-2") : lightbox ? "right-3 sm:right-5" : "right-2",
      )}
    >
      <HugeiconsIcon icon={side === "prev" ? ArrowLeft01Icon : ArrowRight01Icon} strokeWidth={2} />
    </Button>
  );
}

function Slide({ ctx, item, position, active, lightbox }: { ctx: Ctx; item: GalleryItem; position: number; active: boolean; lightbox?: boolean }) {
  const { t } = useMessages();
  if (item.kind === "locked") return <Locked ctx={ctx} item={item} />;
  const f = item.file;
  if (item.kind === "image") {
    if (!f.url) return <Processing>{t("gallery.processingImage")}</Processing>;
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
  const { poster } = videoArt(ctx, f);
  return (
    <VideoPlayer
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
  const [hover, setHover] = useState(false);
  let body: ReactNode;
  if (item.kind === "locked") body = <Locked ctx={ctx} item={item} tile />;
  else if (item.kind === "image")
    body = item.file.url ? (
      <img src={item.file.url} alt="" loading="lazy" decoding="async" draggable={false} className="absolute inset-0 size-full object-cover transition-transform duration-300 motion-safe:group-hover:scale-[1.03]" />
    ) : (
      <Processing>{""}</Processing>
    );
  else {
    const f = item.file;
    const { poster, preview } = videoArt(ctx, f);
    const src = slotSources(poster, 480);
    const base = f.hls && f.name && ctx.hlsBase ? ctx.hlsBase(f) : null;
    body = (
      <>
        {src.src ? (
          <img src={src.src} srcSet={src.srcSet} sizes="(min-width: 768px) 240px, 50vw" alt="" loading="lazy" decoding="async" className="absolute inset-0 size-full object-cover" />
        ) : base ? (
          <SpriteFrame vtt={`${base}sprite.vtt`} xhrSetup={ctx.xhrSetup} fit="cover" />
        ) : null}
        <HoverPreview preview={preview} active={hover} width={240} />
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
      onPointerEnter={() => setHover(true)}
      onPointerLeave={() => setHover(false)}
      onFocus={() => setHover(true)}
      onBlur={() => setHover(false)}
      className="group relative block aspect-square w-full overflow-hidden rounded-md bg-muted outline-none focus-visible:ring-3 focus-visible:ring-ring/60"
      data-ckui="tile"
      data-kind={item.kind}
    >
      {body}
    </button>
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

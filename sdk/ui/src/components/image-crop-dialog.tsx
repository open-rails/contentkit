import { aspectOf, ratio, type AspectRatio } from "../client/aspect.js";
import { Alert02Icon, Image01Icon, RotateClockwiseIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import Cropper, { type Area, type Point } from "react-easy-crop";
import type { Progress as UploadProgress } from "../client/media/client.js";
import { editedSize, rotation, toRotated, type Rotation, type Size } from "../client/crop.js";
import type { CropSource } from "../client/image.js";
import { useMessages } from "../i18n/context.js";
import { useCrop } from "../react/upload.js";
import { editOutput } from "../react/slot.js";
import type { Edit } from "../client/generated/wire.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Progress } from "#ckui/ui/progress";
import { Slider } from "#ckui/ui/slider";
import { progressLabel, progressValue } from "./progress.js";

const MAX_ZOOM = 5;
const ZOOM_STEP = 0.25;

export interface ImageCropDialogProps {
  open: boolean;
  /** Closing (Esc, the close button, Cancel) reports false. */
  onOpenChange: (open: boolean) => void;
  /** The image and its EXIF-oriented original size; edits are in those pixels. */
  source: CropSource | null;
  /** The output's "W:H" (after rotation); "" keeps the image's own shape. */
  aspect: AspectRatio;
  /**
   * Shapes to choose from, "" being the original's (e.g. ["", "1:1", "4:5",
   * "16:9"]). The chooser starts at aspect when listed, else the first.
   */
  aspects?: readonly AspectRatio[];
  /** A small preview of the result beside the controls. Default true. */
  preview?: boolean;
  /** Circular mask (avatars); the saved crop is still the square around it. */
  round?: boolean;
  /** Offer 90° rotation. Default true. */
  rotatable?: boolean;
  /** Starting edit (re-crop); default centred at aspect, unrotated. */
  initialEdit?: Edit | null;
  /** Called with the edit on open and whenever its value changes (safe to pass a state setter). */
  onEditChange?: (edit: Edit | null) => void;
  /** The chosen edit; null is the full centred crop, unrotated. */
  onConfirm: (edit: Edit | null) => void;
  /** Warns when the output is narrower than this many source pixels. */
  targetWidth?: number;
  /** The narrowest edit the server accepts (the preset's Image.MinWidth): zoom stops there, and a smaller image cannot be confirmed. */
  minWidth?: number;
  title?: ReactNode;
  description?: ReactNode;
  confirmLabel?: ReactNode;
  /** A save is running: controls lock and progress shows. */
  busy?: boolean;
  progress?: UploadProgress;
  /** The worker is rendering the outputs (indeterminate progress). */
  rendering?: boolean;
  /** Shown above the footer, e.g. a mapped ContentKitError. */
  error?: ReactNode;
  className?: string;
}

/** Pan, zoom (slider, wheel, pinch, keyboard), rotate and confirm a fixed-aspect edit. */
export function ImageCropDialog(p: ImageCropDialogProps) {
  const { t } = useMessages();
  const busy = !!p.busy;
  return (
    <Dialog open={p.open} onOpenChange={(o) => !busy && p.onOpenChange(o)}>
      <DialogContent className={cn("gap-0 overflow-hidden p-0 sm:max-w-xl", p.className)} showCloseButton={!busy} data-ckui="crop-dialog">
        <DialogHeader className="px-5 pt-5 pb-4 pr-12">
          <DialogTitle>{p.title ?? t("crop.title")}</DialogTitle>
          <DialogDescription>{p.description ?? t("crop.description")}</DialogDescription>
        </DialogHeader>
        {p.source && <CropShapes key={p.source.url} {...p} source={p.source} />}
      </DialogContent>
    </Dialog>
  );
}

// The shape chooser: each choice crops afresh; the initial edit belongs to the starting shape.
function CropShapes(p: ImageCropDialogProps & { source: CropSource }) {
  const { t } = useMessages();
  const shapes = p.aspects?.length ? p.aspects : null;
  const start = shapes && !shapes.includes(p.aspect) ? shapes[0]! : p.aspect;
  const [shape, setShape] = useState(start);
  const chooser = shapes && shapes.length > 1 && (
    <div className="flex items-center gap-2 px-5 pb-3" data-ckui="crop-shapes">
      <span className="text-xs text-muted-foreground">{t("crop.shape")}</span>
      <div className="inline-flex flex-wrap gap-0.5 rounded-lg bg-muted p-0.5" role="group" aria-label={t("crop.shape")}>
        {shapes.map((a) => (
          <button
            key={a || "original"}
            type="button"
            aria-pressed={shape === a}
            disabled={!!p.busy}
            onClick={() => setShape(a)}
            className={cn(
              "inline-flex h-7 items-center rounded-md px-2.5 text-xs font-medium text-muted-foreground tabular-nums transition-colors outline-none hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50",
              shape === a && "bg-background text-foreground shadow-xs",
            )}
          >
            {a || t("crop.original")}
          </button>
        ))}
      </div>
    </div>
  );
  return (
    <>
      {chooser}
      <CropBody key={shape} {...p} aspect={shape} initialEdit={shape === start ? p.initialEdit : null} onReset={shape === start ? undefined : () => setShape(start)} />
    </>
  );
}

function CropBody(p: ImageCropDialogProps & { source: CropSource; onReset?: () => void }) {
  const { t } = useMessages();
  const { source } = p;
  // A native aspect ("") crops at the image's own shape.
  // A native shape ("") is the image's own, turned with it: the whole image fits.
  const [turn, setTurn] = useState<Rotation>(() => rotation(p.initialEdit?.rotate ?? 0));
  const aspect = ratio(p.aspect) ? p.aspect : turn % 180 ? aspectOf(source.height, source.width) : aspectOf(source.width, source.height);
  const size: Size = useMemo(() => ({ width: source.width, height: source.height }), [source.width, source.height]);
  const c = useCrop({ source: size, aspect, initial: p.initialEdit });
  // A native shape turned with the image: crop the whole image again, as the cropper recenters.
  const shaped = useRef(aspect);
  const { setCrop } = c;
  useEffect(() => {
    if (shaped.current === aspect) return;
    shaped.current = aspect;
    setCrop({ x: 0, y: 0, w: size.width, h: size.height });
  }, [aspect, size, setCrop]);
  const [pos, setPos] = useState<Point>({ x: 0, y: 0 });
  const [zoom, setZoom] = useState(1);
  const [interacting, setInteracting] = useState(false);
  const [initial, setInitial] = useState<Area | undefined>(() => (p.initialEdit?.crop ? percentOf(c.crop, size, c.rotate) : undefined));
  const onEditChange = useRef(p.onEditChange);
  onEditChange.current = p.onEditChange;
  const busy = !!p.busy;

  useEffect(() => onEditChange.current?.(c.edit), [c.edit]);

  const out = editOutput(size, c.edit, aspect);
  const min = p.minWidth ?? 0;
  // Zoom z shows 1/z of the widest crop; stop where the crop would fall under min.
  const widest = editOutput(size, { rotate: c.rotate }, aspect).width;
  const tooSmall = widest < min;
  const maxZoom = tooSmall ? 1 : Math.max(1, Math.min(MAX_ZOOM, widest / Math.max(1, min)));
  const belowMin = out.width < min;
  const undersized = !tooSmall && !!(p.targetWidth && out.width < p.targetWidth);
  useEffect(() => {
    if (zoom > maxZoom) setZoom(maxZoom);
  }, [zoom, maxZoom]);

  const complete = (area: Area) => {
    // Before the media has a size the cropper reports NaN areas.
    if (!(area.width > 0 && area.height > 0) || !Number.isFinite(area.x + area.y)) return;
    const shown = editedSize(size, c.rotate);
    const k = (v: number, of: number) => (v / 100) * of;
    c.setFromDisplay(
      { x: k(area.x, shown.width), y: k(area.y, shown.height), w: k(area.width, shown.width), h: k(area.height, shown.height) },
      shown,
    );
  };

  const recenter = () => {
    setInitial(undefined);
    setPos({ x: 0, y: 0 });
    setZoom(1);
  };
  const rotate = (deg: number) => {
    setTurn((r) => rotation(r + deg));
    c.rotateBy(deg);
    recenter();
  };
  const reset = () => {
    if (p.onReset) return p.onReset();
    setTurn(0);
    c.reset();
    recenter();
  };

  const onKeyDown = (e: KeyboardEvent) => {
    if (busy) return;
    if (e.key === "+" || e.key === "=") setZoom((z) => Math.min(maxZoom, z + ZOOM_STEP));
    else if (e.key === "-" || e.key === "_") setZoom((z) => Math.max(1, z - ZOOM_STEP));
    else if (p.rotatable !== false && (e.key === "r" || e.key === "R")) rotate(e.shiftKey ? -90 : 90);
    else return;
    e.preventDefault();
  };

  return (
    <>
      <div
        className="relative h-72 w-full bg-zinc-950 sm:h-80 [&_.reactEasyCrop_CropArea]:border-white/70 [&_.reactEasyCrop_CropArea]:text-black/65"
        onKeyDown={onKeyDown}
        data-ckui="crop-stage"
      >
        <div className="absolute inset-3 sm:inset-4">
          <Cropper
            image={source.url}
            crop={pos}
            zoom={zoom}
            rotation={c.rotate}
            aspect={ratio(aspect)}
            minZoom={1}
            maxZoom={maxZoom}
            zoomSpeed={0.5}
            cropShape={p.round ? "round" : "rect"}
            showGrid={!p.round || interacting}
            objectFit="contain"
            initialCroppedAreaPercentages={initial}
            onCropChange={busy ? noop : setPos}
            onZoomChange={busy ? noop : setZoom}
            onCropComplete={complete}
            onInteractionStart={() => setInteracting(true)}
            onInteractionEnd={() => setInteracting(false)}
            keyboardStep={8}
            disableAutomaticStylesInjection
            mediaProps={{ alt: "", draggable: false }}
            cropperProps={{ "aria-label": t("crop.area"), role: "application" } as never}
          />
        </div>
      </div>

      <div className="grid gap-3 px-5 pt-4">
        <div className="flex items-center gap-1.5">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={t("crop.zoomOut")}
            disabled={busy || zoom <= 1}
            onClick={() => setZoom((z) => Math.max(1, z - ZOOM_STEP))}
          >
            <HugeiconsIcon icon={Image01Icon} strokeWidth={2} className="size-3.5" />
          </Button>
          <Slider
            aria-label={t("crop.zoom")}
            className="mx-1 flex-1"
            min={1}
            max={maxZoom}
            step={0.01}
            value={zoom}
            disabled={busy}
            onValueChange={(v) => setZoom(Array.isArray(v) ? v[0]! : (v as number))}
          />
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={t("crop.zoomIn")}
            disabled={busy || zoom >= maxZoom}
            onClick={() => setZoom((z) => Math.min(maxZoom, z + ZOOM_STEP))}
          >
            <HugeiconsIcon icon={Image01Icon} strokeWidth={2} className="size-5" />
          </Button>
          {p.rotatable !== false && (
            <>
              <span className="mx-1 h-5 w-px bg-border" aria-hidden />
              <Button variant="ghost" size="icon-sm" aria-label={t("crop.rotateLeft")} disabled={busy} onClick={() => rotate(-90)}>
                <HugeiconsIcon icon={RotateClockwiseIcon} strokeWidth={2} />
              </Button>
              <Button variant="ghost" size="icon-sm" aria-label={t("crop.rotateRight")} disabled={busy} onClick={() => rotate(90)}>
                <HugeiconsIcon icon={RotateClockwiseIcon} strokeWidth={2} className="-scale-x-100" />
              </Button>
            </>
          )}
          <Button variant="ghost" size="sm" disabled={busy} onClick={reset} className="text-muted-foreground">
            {t("crop.reset")}
          </Button>
        </div>

        {p.preview !== false && <ResultPreview source={source} crop={c.crop} rotate={c.rotate} round={p.round} size={out} />}
        {tooSmall && (
          <Alert variant="destructive" role="alert" data-ckui="too-small">
            <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
            <AlertDescription>{t("crop.tooSmall", { width: widest, min })}</AlertDescription>
          </Alert>
        )}
        {undersized && (
          <Alert className="border-warning/30 bg-warning/10 text-warning" data-ckui="undersized">
            <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
            <AlertDescription className="text-warning/90">{t("crop.undersized", { width: out.width, target: p.targetWidth! })}</AlertDescription>
          </Alert>
        )}
        {p.error && (
          <Alert variant="destructive" role="alert">
            <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
            <AlertDescription>{p.error}</AlertDescription>
          </Alert>
        )}
        {busy && (
          <Progress value={progressValue(p.progress, p.rendering)} className="gap-2" aria-label={progressLabel(t, p.progress, p.rendering)}>
            <span className="text-xs text-muted-foreground">{progressLabel(t, p.progress, p.rendering)}</span>
          </Progress>
        )}
      </div>

      <DialogFooter className="px-5 pt-4 pb-5">
        <Button variant="outline" disabled={busy} onClick={() => p.onOpenChange(false)}>
          {t("common.cancel")}
        </Button>
        <Button disabled={busy || tooSmall || belowMin} onClick={() => p.onConfirm(c.edit)}>
          {busy ? t("common.saving") : (p.confirmLabel ?? t("common.save"))}
        </Button>
      </DialogFooter>
    </>
  );
}

function noop() {}

const PREVIEW = 72;

// The crop as it will render: the source drawn through the crop, turned.
function ResultPreview({ source, crop, rotate, round, size }: { source: CropSource; crop: { x: number; y: number; w: number; h: number }; rotate: 0 | 90 | 180 | 270; round?: boolean; size: Size }) {
  const { t } = useMessages();
  const turned = rotate % 180 !== 0;
  const k = PREVIEW / Math.max(crop.w, crop.h);
  return (
    <div className="flex items-center gap-3" data-ckui="crop-preview">
      <span
        role="img"
        aria-label={t("crop.preview")}
        className={cn("relative shrink-0 overflow-hidden bg-muted ring-1 ring-border", round ? "rounded-full" : "rounded-sm")}
        style={{ width: (turned ? crop.h : crop.w) * k, height: (turned ? crop.w : crop.h) * k }}
      >
        <span
          className="absolute top-1/2 left-1/2 bg-no-repeat"
          style={{
            width: crop.w * k,
            height: crop.h * k,
            backgroundImage: `url("${source.url}")`,
            backgroundSize: `${source.width * k}px ${source.height * k}px`,
            backgroundPosition: `${-crop.x * k}px ${-crop.y * k}px`,
            transform: `translate(-50%, -50%) rotate(${rotate}deg)`,
          }}
        />
      </span>
      <span className="text-xs text-muted-foreground tabular-nums" data-ckui="crop-size">
        {t("crop.size", { width: size.width, height: size.height })}
      </span>
    </div>
  );
}

/** The crop as percentages of the rotated image, as the cropper takes it. */
function percentOf(crop: { x: number; y: number; w: number; h: number }, source: Size, rot: 0 | 90 | 180 | 270): Area {
  const r = toRotated(crop, source, rot);
  const shown = editedSize(source, rot);
  return { x: (r.x / shown.width) * 100, y: (r.y / shown.height) * 100, width: (r.w / shown.width) * 100, height: (r.h / shown.height) * 100 };
}

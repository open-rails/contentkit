import { aspectOf, ratio, type AspectRatio } from "../aspect.js";
import { Alert02Icon, ArrowLeft01Icon, ArrowRight01Icon, ImageUpload01Icon, Loading03Icon, Video01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useRef, useState, type DragEvent, type ReactNode } from "react";
import type { UploadClient } from "../client.js";
import { useMessages } from "../i18n/context.js";
import { decodeImage, type CropSource } from "../image.js";
import { useErrorReporter, useUploadClient, type UploadUiErrorHandler } from "../provider.js";
import { useFrameStrip, useVideoFrame, useVideoImages, useVideoPoster, type VideoSaveState } from "../video-react.js";
import type { PublicImage } from "../public.js";
import type { FileInfo, ReadResult, RefBody } from "../wire.gen.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Progress } from "#ckui/ui/progress";
import { Slider } from "#ckui/ui/slider";
import { ImageCropDialog } from "./image-crop-dialog.js";
import { progressLabel, progressValue } from "./progress.js";

const FRAME_STEP = 1 / 30;

export interface VideoPickerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The video item. */
  item: RefBody;
  /** The video upload path. Default "source". */
  video?: string;
  /** The poster upload path (its Upload.Frames is the video). Default "poster". */
  path?: string;
  /** The poster's public preset, refetched after a save. */
  image?: PublicImage | null;
  client?: UploadClient;
  /** An editor read of the item from the host; otherwise fetched. */
  read?: ReadResult | null;
  /** Called with the processed poster upload after a save. */
  onChange?: (poster: FileInfo) => void;
  /** Every failure (load, frame grab, save or render); default the provider's. The dialog shows it too. */
  onError?: UploadUiErrorHandler;
  title?: ReactNode;
  className?: string;
}

export interface VideoPosterPickerProps extends VideoPickerProps {
  /** `accept` of the image input. Default "image/*". */
  accept?: string;
  /** Replaces decodeImage (tests). */
  decode?: (file: File) => Promise<CropSource>;
}

/** m:ss.s */
export function formatTime(s: number): string {
  const m = Math.floor(s / 60);
  const sec = s - m * 60;
  return `${m}:${sec < 10 ? "0" : ""}${sec.toFixed(1)}`;
}

/**
 * Sets a video's cover (its poster upload): scrub to an exact frame (a coarse
 * strip, a slider and frame steps over GET /frame) and use it whole or
 * cropped (a frame op), or upload an image and crop it at the video's aspect
 * (a put), or let the worker choose the frame. Covers keep the video's aspect.
 */
export function VideoPosterPicker(p: VideoPosterPickerProps) {
  const { t } = useMessages();
  const [saving, setSaving] = useState(false);
  return (
    <Dialog open={p.open} onOpenChange={(o) => !saving && p.onOpenChange(o)}>
      <DialogContent className={cn("gap-0 overflow-hidden p-0 sm:max-w-2xl", p.className)} showCloseButton={!saving} data-ckui="poster-picker">
        <DialogHeader className="px-5 pt-5 pb-4 pr-12">
          <DialogTitle>{p.title ?? t("poster.title")}</DialogTitle>
          <DialogDescription>{t("poster.description")}</DialogDescription>
        </DialogHeader>
        {p.open && <PosterBody {...p} onSaving={setSaving} />}
      </DialogContent>
    </Dialog>
  );
}

function PosterBody(p: VideoPosterPickerProps & { onSaving: (b: boolean) => void }) {
  const { t, error: errorText } = useMessages();
  const client = useUploadClient(p.client);
  const report = useErrorReporter(p.onError);
  const loaded = useVideoImages(client, { ref: p.item, video: p.video, poster: p.path, read: p.read });
  useEffect(() => void (loaded.error && report(loaded.error, "poster.load")), [loaded.error, report]);
  const { video, poster: current } = loaded;
  const selection = current?.frame;
  const [mode, setMode] = useState<"frame" | "upload">(current && !selection ? "upload" : "frame");
  const [time, setTime] = useState<number>();
  const [crop, setCrop] = useState<CropSource | null>(null);
  const [cropFor, setCropFor] = useState<"frame" | "upload">("frame");
  const [decodeError, setDecodeError] = useState<string>();
  const input = useRef<HTMLInputElement>(null);
  const duration = video && !video.staged ? (video.dur ?? 0) : 0;
  const aspect = video?.w && video.h ? aspectOf(video.w, video.h) : "16:9";
  const shown = time ?? selection?.t ?? duration * 0.25;
  // Frames come from the placed video (GET /frame answers conflict while it is staged).
  const path = duration > 0 ? video!.path : undefined;
  const poster = useVideoPoster(client, {
    ref: p.item,
    path: p.path,
    image: p.image,
    onSaved: (f) => {
      loaded.set(f);
      p.onChange?.(f);
      p.onOpenChange(false);
    },
    onError: (e) => report(e, "poster.save"),
  });
  const busy = poster.state.status === "saving";
  const onSaving = useRef(p.onSaving);
  onSaving.current = p.onSaving;
  useEffect(() => onSaving.current(busy), [busy]);
  const onFrameError = (e: unknown) => report(e, "poster.frame");
  const frame = useVideoFrame(client, { ref: p.item, path, time: duration > 0 ? shown : undefined, width: 960, onError: onFrameError });
  const strip = useFrameStrip(client, { ref: p.item, path, duration, count: 8, width: 160, onError: onFrameError });
  useEffect(() => () => crop?.revoke?.(), [crop]);

  const openCrop = async (source: "frame" | "upload", f?: File) => {
    setDecodeError(undefined);
    try {
      if (source === "upload") setCrop(await (p.decode ?? decodeImage)(f!));
      else {
        const blob = await client.getFrame(p.item, path!, shown, 1280);
        const url = URL.createObjectURL(blob);
        // Frame edits are in the frame's pixels (the video's w×h), whatever the preview's size.
        setCrop({ url, width: video!.w!, height: video!.h!, revoke: () => URL.revokeObjectURL(url) });
      }
      setCropFor(source);
    } catch (e) {
      setDecodeError(errorText(e));
      report(e, source === "upload" ? "slot.decode" : "poster.frame");
    }
  };
  const pickFile = (f?: File) => f && void openCrop("upload", f);
  const [over, setOver] = useState(false);
  const drop = {
    onDragOver: (e: DragEvent) => {
      if (busy || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      setOver(true);
    },
    onDragLeave: () => setOver(false),
    onDrop: (e: DragEvent) => {
      e.preventDefault();
      setOver(false);
      pickFile(e.dataTransfer.files[0]);
    },
  };
  const failed = poster.state.status === "error" ? poster.state.error : (loaded.error ?? (frame.error && !frame.url ? frame.error : undefined));
  const error = decodeError ?? (failed ? errorText(failed) : undefined);

  return (
    <>
      <div className="grid gap-4 px-5">
        <div className="inline-flex w-fit rounded-lg bg-muted p-0.5" role="tablist" aria-label={t("poster.title")}>
          {(["frame", "upload"] as const).map((m) => (
            <button
              key={m}
              type="button"
              role="tab"
              aria-selected={mode === m}
              disabled={busy}
              onClick={() => setMode(m)}
              className={cn(
                "inline-flex h-7 items-center gap-1.5 rounded-md px-3 text-sm font-medium text-muted-foreground transition-colors outline-none focus-visible:ring-3 focus-visible:ring-ring/50",
                mode === m && "bg-background text-foreground shadow-xs",
              )}
            >
              <HugeiconsIcon icon={m === "frame" ? Video01Icon : ImageUpload01Icon} strokeWidth={2} className="size-4" />
              {t(m === "frame" ? "poster.frame" : "poster.upload")}
            </button>
          ))}
        </div>

        {mode === "frame" &&
          (loaded.loading && !video ? (
            <Stage loading aspect={aspect} />
          ) : !duration ? (
            <p className="rounded-lg bg-muted px-4 py-6 text-center text-sm text-muted-foreground" data-ckui="processing">
              {t("poster.processing")}
            </p>
          ) : (
            <>
              <Stage url={frame.url} loading={frame.loading || (!frame.url && !frame.error)} aspect={aspect} />
              <div className="grid gap-2">
                <div className="flex items-center gap-1.5">
                  <Button variant="ghost" size="icon-sm" aria-label={t("poster.previousFrame")} disabled={busy || shown <= 0} onClick={() => setTime(Math.max(0, shown - FRAME_STEP))}>
                    <HugeiconsIcon icon={ArrowLeft01Icon} strokeWidth={2} />
                  </Button>
                  <Slider
                    aria-label={t("poster.time")}
                    className="mx-1 flex-1"
                    min={0}
                    max={duration}
                    step={0.01}
                    value={shown}
                    disabled={busy}
                    onValueChange={(v) => setTime(Array.isArray(v) ? v[0]! : (v as number))}
                  />
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={t("poster.nextFrame")}
                    disabled={busy || shown >= duration}
                    onClick={() => setTime(Math.min(duration, shown + FRAME_STEP))}
                  >
                    <HugeiconsIcon icon={ArrowRight01Icon} strokeWidth={2} />
                  </Button>
                  <span className="w-14 text-right text-xs text-muted-foreground tabular-nums" data-ckui="time">
                    {formatTime(shown)}
                  </span>
                </div>
                <div className="grid grid-cols-8 gap-1" data-ckui="frame-strip">
                  {strip.map((f) => {
                    const near = Math.abs(f.time - shown) <= duration / 16;
                    return (
                      <button
                        key={f.time}
                        type="button"
                        disabled={busy}
                        aria-label={t("poster.jumpTo", { time: formatTime(f.time) })}
                        onClick={() => setTime(f.time)}
                        style={{ aspectRatio: String(ratio(aspect)) }}
                        className={cn(
                          "relative overflow-hidden rounded-md bg-muted outline-none ring-offset-1 ring-offset-background focus-visible:ring-2 focus-visible:ring-ring",
                          near && "ring-2 ring-primary",
                        )}
                      >
                        {f.url && <img src={f.url} alt="" className="absolute inset-0 size-full object-contain" />}
                      </button>
                    );
                  })}
                </div>
              </div>
            </>
          ))}

        {mode === "upload" && (
          <label
            className={cn(
              "flex aspect-video cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border text-sm text-muted-foreground transition-colors hover:border-primary hover:bg-primary/5",
              over && "border-primary bg-primary/5",
              busy && "pointer-events-none opacity-60",
            )}
            data-ckui="poster-drop"
            {...drop}
          >
            <HugeiconsIcon icon={ImageUpload01Icon} strokeWidth={1.5} className="size-8" />
            {t("poster.drop")}
            <input
              ref={input}
              type="file"
              accept={p.accept ?? "image/*"}
              className="sr-only"
              data-ckui="file"
              disabled={busy}
              onChange={(e) => {
                const f = e.target.files?.[0];
                e.target.value = "";
                pickFile(f);
              }}
            />
          </label>
        )}

        {error && (
          <Alert variant="destructive" role="alert">
            <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <SaveProgress state={poster.state} label={t("poster.rendering")} />
      </div>

      <DialogFooter className="px-5 pt-4 pb-5 sm:justify-between">
        <Button variant="ghost" disabled={busy || !!selection?.auto} onClick={() => void poster.saveAuto()} title={t("poster.autoHint")}>
          {t("poster.auto")}
        </Button>
        <div className="flex flex-col-reverse gap-2 sm:flex-row">
          <Button variant="outline" disabled={busy} onClick={() => p.onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          {mode === "frame" && duration > 0 && (
            <>
              <Button variant="outline" disabled={busy} onClick={() => void openCrop("frame")}>
                {t("poster.crop")}
              </Button>
              <Button disabled={busy} onClick={() => void poster.saveFrame(shown)}>
                {busy ? t("common.saving") : t("poster.useFrame")}
              </Button>
            </>
          )}
        </div>
      </DialogFooter>

      <ImageCropDialog
        open={!!crop}
        onOpenChange={(o) => !o && !busy && setCrop(null)}
        source={crop}
        aspect={aspect}
        rotatable={cropFor === "upload"}
        targetWidth={1920}
        title={t("poster.cropTitle")}
        busy={busy}
        progress={poster.state.status === "saving" ? poster.state.progress : undefined}
        rendering={poster.state.status === "saving" && poster.state.rendering}
        error={poster.state.status === "error" && crop ? errorText(poster.state.error) : undefined}
        onConfirm={(edit) => {
          if (cropFor === "frame") void poster.saveFrame(shown, edit);
          else if (crop?.file) void poster.saveUpload(crop.file, edit);
        }}
      />
    </>
  );
}

function Stage({ url, loading, aspect: shape }: { url?: string; loading?: boolean; aspect: AspectRatio }) {
  const aspect = ratio(shape) ?? 16 / 9;
  return (
    <div className="relative mx-auto max-h-[50svh] max-w-full overflow-hidden rounded-lg bg-zinc-950" style={{ aspectRatio: String(aspect), width: `min(100%, calc(50svh * ${aspect}))` }} data-ckui="frame-stage">
      {url && <img src={url} alt="" className="absolute inset-0 size-full object-contain" />}
      {loading && (
        <div className="absolute inset-0 flex items-center justify-center text-white/70">
          <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="size-6 animate-spin" />
        </div>
      )}
    </div>
  );
}

export function SaveProgress({ state, label }: { state: VideoSaveState; label: string }) {
  const { t } = useMessages();
  if (state.status !== "saving") return null;
  const indeterminate = state.rendering || !state.progress;
  const text = indeterminate ? label : progressLabel(t, state.progress, false);
  return (
    <Progress value={indeterminate ? null : progressValue(state.progress, false)} className="gap-2" aria-label={text} data-ckui="save-progress">
      <span className="text-xs text-muted-foreground">{text}</span>
    </Progress>
  );
}

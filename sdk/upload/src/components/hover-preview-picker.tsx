import { Alert02Icon, Loading03Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useRef, useState } from "react";
import type { UploadClient } from "../client.js";
import { useMessages } from "../i18n/context.js";
import { useUploadClient } from "../provider.js";
import { round3, useFrameStrip, useHoverSection, useVideoImages } from "../video-react.js";
import { HOVER_PREVIEW_MAX, HOVER_PREVIEW_MIN, type RefBody, type VideoImages } from "../wire.gen.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Slider } from "#ckui/ui/slider";
import { HoverPreview, useReducedMotion } from "./video-poster.js";
import { formatTime, SaveProgress, type VideoPickerProps } from "./video-poster-picker.js";

const APPROX_FRAMES = 6;

export type HoverPreviewPickerProps = VideoPickerProps;

/**
 * Picks the hover-preview section: a range over a frame strip of the whole
 * video (1–6 s), an approximate flip-book of the section while choosing, and
 * the rendered loop once saved.
 */
export function HoverPreviewPicker(p: HoverPreviewPickerProps) {
  const { t } = useMessages();
  const [saving, setSaving] = useState(false);
  return (
    <Dialog open={p.open} onOpenChange={(o) => !saving && p.onOpenChange(o)}>
      <DialogContent className={cn("gap-0 overflow-hidden p-0 sm:max-w-2xl", p.className)} showCloseButton={!saving} data-ckui="preview-picker">
        <DialogHeader className="px-5 pt-5 pb-4 pr-12">
          <DialogTitle>{p.title ?? t("preview.title")}</DialogTitle>
          <DialogDescription>{t("preview.description", { min: HOVER_PREVIEW_MIN, max: HOVER_PREVIEW_MAX })}</DialogDescription>
        </DialogHeader>
        {p.open && <Loader {...p} onSaving={setSaving} />}
      </DialogContent>
    </Dialog>
  );
}

function Loader(p: HoverPreviewPickerProps & { onSaving: (b: boolean) => void }) {
  const { t, error } = useMessages();
  const client = useUploadClient(p.client);
  const loaded = useVideoImages(client, { ref: p.item, file: p.file, images: p.images });
  const video = loaded.images?.video;
  if (!loaded.images || !video?.encoded) {
    return (
      <div className="grid gap-4 px-5 pb-5">
        {loaded.loading ? (
          <div className="flex aspect-video items-center justify-center rounded-lg bg-zinc-950 text-white/70">
            <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="size-6 animate-spin" />
          </div>
        ) : (
          <p className="rounded-lg bg-muted px-4 py-6 text-center text-sm text-muted-foreground" data-ckui="processing">
            {loaded.error ? error(loaded.error) : t("poster.processing")}
          </p>
        )}
      </div>
    );
  }
  return <SectionBody {...p} client={client} images={loaded.images} onLoaded={loaded.set} />;
}

function SectionBody(p: HoverPreviewPickerProps & { client: UploadClient; images: VideoImages; onLoaded: (v: VideoImages) => void; onSaving: (b: boolean) => void }) {
  const { t, error: errorText } = useMessages();
  const { client, images } = p;
  const video = images.video!;
  const file = p.file ?? video.file;
  const current = images.hover_preview.selection;
  const s = useHoverSection(client, {
    ref: p.item,
    file,
    duration: video.duration,
    initial: current,
    onSaved: (v) => {
      p.onLoaded(v);
      p.onChange?.(v);
      p.onOpenChange(false);
    },
  });
  const busy = s.state.status === "saving";
  const onSaving = useRef(p.onSaving);
  onSaving.current = p.onSaving;
  useEffect(() => onSaving.current(busy), [busy]);
  const strip = useFrameStrip(client, { ref: p.item, file, duration: video.duration, count: 10, width: 128 });
  const unchanged = !!current && Math.abs(current.start - s.start) < 0.01 && Math.abs(current.duration - s.length) < 0.01;
  const rendered = unchanged && !images.hover_preview.pending && images.hover_preview.mp4.length + images.hover_preview.webp.length > 0;
  const end = s.start + s.length;
  const pct = (v: number) => `${(v / video.duration) * 100}%`;

  return (
    <>
      <div className="grid gap-4 px-5">
        <div className="relative aspect-video overflow-hidden rounded-lg bg-zinc-950" data-ckui="preview-stage">
          {rendered ? (
            <HoverPreview preview={images.hover_preview} active width={640} />
          ) : (
            <FlipBook client={client} item={p.item} file={file} start={s.start} length={s.length} />
          )}
          <span className="absolute top-2 left-2 rounded bg-black/60 px-1.5 py-0.5 text-[11px] font-medium text-white">
            {t(rendered ? "preview.rendered" : "preview.approximate")}
          </span>
        </div>

        <div className="grid gap-2">
          <div className="relative grid grid-cols-10 gap-0.5 overflow-hidden rounded-md" data-ckui="frame-strip">
            {strip.map((f) => (
              <div key={f.time} className="relative aspect-video bg-muted">
                {f.url && <img src={f.url} alt="" className="absolute inset-0 size-full object-cover" />}
              </div>
            ))}
            <div className="pointer-events-none absolute inset-y-0 left-0 bg-black/55" style={{ width: pct(s.start) }} />
            <div className="pointer-events-none absolute inset-y-0 right-0 bg-black/55" style={{ left: pct(end) }} />
            <div className="pointer-events-none absolute inset-y-0 rounded-sm border-2 border-primary" style={{ left: pct(s.start), width: pct(s.length) }} data-ckui="section" />
          </div>
          <Slider
            aria-label={t("preview.section")}
            min={0}
            max={video.duration}
            step={0.05}
            value={[s.start, end]}
            disabled={busy}
            onValueChange={(v, details) => {
              const [a, b] = v as number[];
              // Dragging the end resizes; dragging the start moves the section.
              if (details.activeThumbIndex === 1) s.setLength(round3(b! - s.start));
              else s.setStart(a!);
            }}
          />
          <div className="flex items-center justify-between text-xs text-muted-foreground tabular-nums">
            <span data-ckui="start">{t("preview.start", { time: formatTime(s.start) })}</span>
            <span data-ckui="length">{t("preview.length", { seconds: s.length.toFixed(1) })}</span>
          </div>
        </div>

        {s.state.status === "error" && (
          <Alert variant="destructive" role="alert">
            <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
            <AlertDescription>{errorText(s.state.error)}</AlertDescription>
          </Alert>
        )}
        <SaveProgress state={s.state} label={t("preview.rendering")} />
      </div>

      <DialogFooter className="px-5 pt-4 pb-5 sm:justify-between">
        <Button variant="ghost" disabled={busy || !current || !!current.auto} onClick={() => void s.saveAuto()}>
          {t("preview.auto")}
        </Button>
        <div className="flex flex-col-reverse gap-2 sm:flex-row">
          <Button variant="outline" disabled={busy} onClick={() => p.onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={busy || unchanged} onClick={() => void s.save()}>
            {busy ? t("common.saving") : t("preview.save")}
          </Button>
        </div>
      </DialogFooter>
    </>
  );
}

/** A few frames of the section, cycled: the chosen loop before it is rendered. */
function FlipBook({ client, item, file, start, length }: { client: UploadClient; item: RefBody; file: string; start: number; length: number }) {
  const reduced = useReducedMotion();
  const [frames, setFrames] = useState<{ key: string; urls: string[] }>({ key: "", urls: [] });
  const [i, setI] = useState(0);
  const key = `${item.kind}/${item.id}/${item.version ?? ""}|${file}|${start}|${length}`;
  const ref = useRef(item);
  ref.current = item;
  useEffect(() => {
    const ctl = new AbortController();
    const urls: string[] = [];
    const t = setTimeout(async () => {
      for (let n = 0; n < APPROX_FRAMES; n++) {
        try {
          const blob = await client.getFrame(ref.current, round3(start + (length * n) / APPROX_FRAMES), { file, width: 640, signal: ctl.signal });
          if (ctl.signal.aborted) return;
          urls.push(URL.createObjectURL(blob));
          setFrames({ key, urls: [...urls] });
        } catch {
          if (ctl.signal.aborted) return;
        }
      }
    }, 250);
    return () => {
      clearTimeout(t);
      ctl.abort();
      urls.forEach((u) => URL.revokeObjectURL(u));
    };
  }, [client, file, start, length, key]);
  const urls = frames.key === key ? frames.urls : [];
  useEffect(() => {
    if (reduced || urls.length < 2) return;
    const id = setInterval(() => setI((x) => x + 1), (length * 1000) / APPROX_FRAMES);
    return () => clearInterval(id);
  }, [reduced, urls.length, length]);
  const url = urls.length ? urls[reduced ? 0 : i % urls.length] : undefined;
  return (
    <>
      {url && <img src={url} alt="" className="absolute inset-0 size-full object-cover" data-ckui="flipbook" />}
      {urls.length < APPROX_FRAMES && (
        <div className="absolute right-2 bottom-2 text-white/70">
          <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="size-4 animate-spin" />
        </div>
      )}
    </>
  );
}

import { cn } from "cn";
import { useEffect, useRef, useState } from "react";
import { PauseIcon, PlayIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import type { ContentKitClient } from "../../client/client.js";
import type { ReaderPlan } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "#ckui/ui/dialog";

/** One choosable page: its original source hash, display label and thumbnail. */
export interface PickerPage {
  id: string;
  label: string;
  url?: string;
  /** Visible but unavailable as a selection, for example an ambiguous chapter anchor. */
  disabled?: boolean;
}

/** A non-page choice shown above the grid (an anchor, "start"). */
export interface PickerExtra {
  value: string;
  label: string;
}

export interface PlanMedia {
  /** Each published page in reading order, with its thumbnail URL once read. */
  pages: PickerPage[];
  /** Audio track name → playable URL. */
  audio: ReadonlyMap<string, string>;
}

// The smallest rendition: "thumb" when configured, else the last preset by name.
const thumbOf = (files: Record<string, string>) => files.thumb ?? files[Object.keys(files).sort().at(-1) ?? ""];
const dir = (path: string) => path.slice(0, path.lastIndexOf("/") + 1);

/** A reader plan's pages with thumbnails and its audio with URLs, read once per plan (thumbnails are a convenience: names work without them). */
export function usePlanMedia(plan: ReaderPlan | null, client: ContentKitClient): PlanMedia {
  const allPages = (plan?.segments ?? []).flatMap((s) => s.pages ?? []);
  const [urls, setUrls] = useState<{ plan: ReaderPlan | null; pages: Map<string, string>; audio: Map<string, string> }>({ plan: null, pages: new Map(), audio: new Map() });
  useEffect(() => {
    if (!plan) return;
    const ctrl = new AbortController();
    const pages = new Map<string, string>(),
      audio = new Map<string, string>();
    const reads = plan.segments.flatMap((s) => {
      const first = s.pages?.[0] && thumbOf(s.pages[0].files);
      if (!s.item || !first) return [];
      return [
        client.media.read(s.item, { prefix: dir(first), limit: s.pages!.length, signal: ctrl.signal }).then((r) => {
          const byPath = new Map(r.files.flatMap((f) => (f.url ? [[f.path, f.url] as const] : [])));
          for (const p of s.pages ?? []) {
            const url = byPath.get(thumbOf(p.files) ?? "");
            if (url) pages.set(p.id, url);
          }
        }),
      ];
    });
    const playback = plan.playback;
    if (playback?.tracks.length)
      reads.push(
        client.media.read(playback.item, { limit: playback.tracks.length, signal: ctrl.signal }).then((r) => {
          const byPath = new Map(r.files.flatMap((f) => (f.url ? [[f.path, f.url] as const] : [])));
          for (const t of playback.tracks) {
            const url = byPath.get(t.path);
            if (url) audio.set(t.name, url);
          }
        }),
      );
    void Promise.allSettled(reads).then(() => !ctrl.signal.aborted && setUrls({ plan, pages, audio }));
    return () => ctrl.abort();
  }, [plan, client]);
  const ready = urls.plan === plan;
  const counts = new Map<string, number>();
  for (const p of allPages) counts.set(p.id, (counts.get(p.id) ?? 0) + 1);
  return { pages: allPages.map((p) => ({ id: p.id, label: p.name, disabled: counts.get(p.id) !== 1, url: ready ? urls.pages.get(p.id) : undefined })), audio: ready ? urls.audio : new Map() };
}

/**
 * Picks one page from thumbnails: the trigger shows the current page, the
 * dialog the whole version in reading order. value is a source hash or one of
 * extras' values.
 */
export function PagePicker({ label, pages, value, extras = [], onChange, className }: {
  label: string;
  pages: readonly PickerPage[];
  value: string;
  extras?: readonly PickerExtra[];
  onChange: (value: string) => void;
  className?: string;
}) {
  const { t } = useMessages();
  const [open, setOpen] = useState(false);
  const index = pages.findIndex((p) => p.id === value && !p.disabled);
  const current = index >= 0 ? pages[index] : undefined;
  const extra = extras.find((e) => e.value === value);
  const choose = (v: string) => {
    onChange(v);
    setOpen(false);
  };
  return (
    <>
      <Button type="button" size="sm" variant="outline" aria-label={label} className={cn("h-auto gap-2 py-1 ps-1", className)} onClick={() => setOpen(true)} data-ckui="page-picker">
        <span className="flex h-10 w-7 shrink-0 items-center justify-center overflow-hidden rounded-sm bg-muted text-[10px] tabular-nums text-muted-foreground">
          {current?.url ? <img src={current.url} alt="" className="size-full object-cover" /> : current ? index + 1 : null}
        </span>
        <span className="tabular-nums">{extra ? extra.label : current ? t("pagePicker.page", { n: index + 1 }) : t("pagePicker.choose")}</span>
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-3xl" data-ckui="page-picker-dialog">
          <DialogHeader>
            <DialogTitle>{label}</DialogTitle>
          </DialogHeader>
          {extras.length > 0 && (
            <div className="flex flex-wrap gap-2">
              {extras.map((e) => (
                <Button key={e.value} type="button" size="sm" variant={e.value === value ? "default" : "outline"} onClick={() => choose(e.value)}>
                  {e.label}
                </Button>
              ))}
            </div>
          )}
          <ul className="grid max-h-[60vh] grid-cols-[repeat(auto-fill,minmax(5.5rem,1fr))] gap-2 overflow-y-auto p-1">
            {pages.map((p, i) => (
              <li key={`${p.id}:${i}`}>
                <button
                  type="button"
                  aria-label={t("pagePicker.pageNamed", { n: i + 1, name: p.label })}
                  aria-pressed={p.id === value}
                  disabled={p.disabled}
                  className={cn(
                    "grid w-full gap-1 rounded-md p-1 text-center text-xs tabular-nums outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40",
                    p.id === value && "bg-muted ring-2 ring-primary",
                  )}
                  onClick={() => choose(p.id)}
                >
                  <span className="flex aspect-[7071/10000] items-center justify-center overflow-hidden rounded-sm bg-muted text-muted-foreground">
                    {p.url ? <img src={p.url} alt="" loading="lazy" className="size-full object-cover" /> : i + 1}
                  </span>
                  {i + 1}
                </button>
              </li>
            ))}
          </ul>
        </DialogContent>
      </Dialog>
    </>
  );
}

/** A play/pause button for one audio URL; one preview plays at a time. */
export function AudioPreview({ url, label }: { url?: string; label: string }) {
  const { t } = useMessages();
  const audio = useRef<HTMLAudioElement | null>(null);
  const [playing, setPlaying] = useState(false);
  useEffect(() => () => audio.current?.pause(), [url]);
  if (!url) return null;
  const toggle = () => {
    const a = audio.current;
    if (!a) return;
    if (a.paused) {
      for (const other of document.querySelectorAll<HTMLAudioElement>("audio[data-ckui=audio-preview]")) if (other !== a) other.pause();
      void a.play().catch(() => setPlaying(false));
    } else a.pause();
  };
  return (
    <>
      <Button type="button" size="icon-sm" variant="ghost" aria-label={t(playing ? "pagePicker.pause" : "pagePicker.play", { name: label })} aria-pressed={playing} onClick={toggle}>
        <HugeiconsIcon icon={playing ? PauseIcon : PlayIcon} strokeWidth={2} className="size-4" />
      </Button>
      <audio ref={audio} src={url} preload="none" data-ckui="audio-preview" onPlay={() => setPlaying(true)} onPause={() => setPlaying(false)} onEnded={() => setPlaying(false)} />
    </>
  );
}

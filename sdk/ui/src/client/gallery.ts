import type { FileInfo, ReadResult } from "./generated/wire.js";

export type GalleryView = "carousel" | "grid";

export interface GalleryMediaItem {
  kind: "image" | "video" | "audio";
  key: string;
  /** An image; a ladder's widest video track (its size and duration) or its audio file. */
  file: FileInfo;
  /** A video or audio ladder's HLS folder from the read's `hls`, e.g. "hls/". */
  dir?: string;
  /** Width / height from the read API; a default when unknown. */
  aspect: number;
}

/** What a viewer without access is missing; `backdrop` is the public preview drawn behind it. */
export interface GalleryLockedItem {
  kind: "locked";
  key: string;
  count: number;
  videos: number;
  backdrop?: string;
  aspect: number;
}

export type GalleryItem = GalleryMediaItem | GalleryLockedItem;

export const isVideoType = (type?: string) => !!type?.startsWith("video/");
export const isAudioType = (type?: string) => !!type?.startsWith("audio/");
export const isImageType = (type?: string) => !!type?.startsWith("image/");

/** The stage aspect of an audio slide. */
export const AUDIO_ASPECT = 3;

/** Subtitle files are a video's text tracks, not gallery items. */
export const isSubtitleType = (type?: string) =>
  ["text/vtt", "application/x-subrip", "text/x-ssa", "text/x-ass"].includes(type ?? "");

function aspectOf(f: FileInfo | undefined, fallback: number) {
  return f?.w && f.h ? f.w / f.h : fallback;
}

const parent = (path: string) => path.slice(0, path.lastIndexOf("/") + 1);

/**
 * The read result as gallery items in manifest order. With access: each
 * image, each playable HLS ladder (a folder in `hls`: a video, or an
 * audio-only one) and each other audio file. Without: the item's public
 * previews (the read's `previews`), then one locked item for the rest; an
 * item's private files are all or nothing, so locked files never carry URLs.
 * Other files (plain videos, subtitles, zips) are not items; scope the read
 * with a prefix to choose.
 */
export function galleryItems(read: ReadResult | null | undefined): GalleryItem[] {
  if (!read) return [];
  const full = read.access === "full";
  const dirs = read.hls ?? [];
  const dirOf = (f: FileInfo) => dirs.find((d) => f.path.startsWith(d));
  // A locked ladder lists its tracks (and sprite): count each folder once.
  const unit = (f: FileInfo) => parent(f.path) || f.path;
  const lockedMedia = read.files.filter((f) => f.locked && (isVideoType(f.type) || isAudioType(f.type)));
  const units = new Set(lockedMedia.map(unit));
  const lockedImages = read.files.filter((f) => f.locked && isImageType(f.type) && !units.has(parent(f.path)));
  const lockedVideos = new Set(lockedMedia.filter((f) => isVideoType(f.type)).map(unit)).size;
  const previews = full ? [] : (read.previews ?? []);
  // Each preview shows one of the locked images.
  const lockedCount = Math.max(lockedImages.length - previews.length, 0) + units.size;
  const items: GalleryItem[] = previews.map((url, i) => ({ kind: "image", key: `preview/${i + 1}`, file: { path: `preview/${i + 1}`, type: "image/webp", url }, aspect: 1 }));
  const seen = new Set<string>();
  for (const f of read.files) {
    if (f.locked) continue;
    const dir = dirOf(f);
    if (dir) {
      if (seen.has(dir)) continue;
      seen.add(dir);
      const files = read.files.filter((x) => x.path.startsWith(dir));
      const video = files.filter((x) => isVideoType(x.type)).reduce<FileInfo | undefined>((a, x) => ((x.w ?? 0) > (a?.w ?? -1) ? x : a), undefined);
      if (video) {
        const dur = video.dur ?? files.find((x) => x.dur)?.dur;
        items.push({ kind: "video", key: dir, dir, file: { ...video, dur }, aspect: aspectOf(video, 16 / 9) });
      } else {
        const audio = files.find((x) => x.path.endsWith(".m4a")) ?? files.find((x) => isAudioType(x.type));
        if (audio) items.push({ kind: "audio", key: dir, dir, file: audio, aspect: AUDIO_ASPECT });
      }
      continue;
    }
    if (isImageType(f.type)) items.push({ kind: "image", key: f.path, file: f, aspect: aspectOf(f, 1) });
    else if (isAudioType(f.type)) items.push({ kind: "audio", key: f.path, file: f, aspect: AUDIO_ASPECT });
  }
  if (lockedCount > 0)
    items.push({ kind: "locked", key: "locked", count: lockedCount, videos: lockedVideos, backdrop: previews.at(-1), aspect: 1 });
  return items;
}

/** The carousel stage's width / height: the current item's native aspect. */
export function stageAspect(items: readonly GalleryItem[], index = 0): number {
  return items[index]?.aspect ?? 1;
}

/** "0:42", "12:05", "1:02:09". */
export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

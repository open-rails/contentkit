import type { FileInfo, ReadResult } from "./wire.gen.js";

export type GalleryView = "carousel" | "grid";

export interface GalleryMediaItem {
  kind: "image" | "video" | "audio";
  key: string;
  file: FileInfo;
  /** Width / height from the read API; a default when unknown. */
  aspect: number;
}

/** What a viewer without access is missing; `teaser` is the blurred image drawn behind it. */
export interface GalleryLockedItem {
  kind: "locked";
  key: string;
  count: number;
  videos: number;
  teaser?: FileInfo;
  aspect: number;
}

export type GalleryItem = GalleryMediaItem | GalleryLockedItem;

export const isVideoType = (type?: string) => !!type?.startsWith("video/");
export const isAudioType = (type?: string) => !!type?.startsWith("audio/");

/** An audio file's M4A variant name: request it in the read (`variants`) for `<audio>` playback. */
export const AUDIO_VARIANT = "audio";

/** An audio file's download key in `read.downloads`. */
export const audioDownloadKey = (name: string) => `${name}-audio`;

/** The stage aspect of an audio slide. */
export const AUDIO_ASPECT = 3;

/** Subtitle sidecars are a video's text tracks, not gallery items. */
export const isSubtitleType = (type?: string) =>
  ["text/vtt", "application/x-subrip", "text/x-ssa", "text/x-ass"].includes(type ?? "");

function aspectOf(f: FileInfo | undefined, fallback: number) {
  return f?.w && f.h ? f.w / f.h : fallback;
}

/**
 * The read result as gallery items in manifest order: every file this viewer
 * may see (full access hides the teaser), then one locked item for the rest.
 * Locked files never carry URLs; the teaser is the only image behind the lock.
 */
export function galleryItems(read: ReadResult | null | undefined): GalleryItem[] {
  if (!read) return [];
  const full = read.access === "full";
  const files = read.files.filter((f) => !isSubtitleType(f.type));
  const locked = files.filter((f) => f.locked);
  const teaser = full || locked.length === 0 ? undefined : files.find((f) => f.teaser && !f.locked && f.url);
  const items: GalleryItem[] = [];
  for (const f of files) {
    if (f.locked || (f.teaser && (full || f === teaser))) continue;
    if (isAudioType(f.type)) {
      items.push({ kind: "audio", key: `f${f.index}`, file: f, aspect: AUDIO_ASPECT });
      continue;
    }
    const video = isVideoType(f.type);
    items.push({ kind: video ? "video" : "image", key: `f${f.index}`, file: f, aspect: aspectOf(f, video ? 16 / 9 : 1) });
  }
  if (locked.length > 0)
    items.push({
      kind: "locked",
      key: "locked",
      count: locked.length,
      videos: locked.filter((f) => isVideoType(f.type)).length,
      teaser,
      aspect: aspectOf(teaser, 1),
    });
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

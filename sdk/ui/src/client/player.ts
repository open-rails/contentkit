// Player logic without the DOM: the keyboard map, resume points, seek-sprite
// cues, track choice, played time and the viewer's remembered settings.

/** What a key does to the player. */
export type PlayerAction =
  | { type: "toggle" }
  | { type: "seekBy"; seconds: number }
  /** A fraction of the duration: Home 0, End 1, digit n n/10. */
  | { type: "seekTo"; fraction: number }
  | { type: "volumeBy"; delta: number }
  | { type: "mute" }
  | { type: "captions" }
  | { type: "fullscreen" }
  | { type: "theater" }
  | { type: "miniPlayer" }
  | { type: "speed"; step: 1 | -1 };

export interface KeyLike {
  key: string;
  altKey?: boolean;
  ctrlKey?: boolean;
  metaKey?: boolean;
}

/** Seconds the arrow keys seek; J and L, and a double tap, seek twice as far. */
export const ARROW_SEEK = 5;
export const JUMP_SEEK = 10;
export const VOLUME_STEP = 0.05;
export const PLAYBACK_SPEEDS = [0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2] as const;

/**
 * The shortcut for a key, as on YouTube: Space/K play, ←/→ 5 s, J/L 10 s,
 * ↑/↓ volume, M mute, C subtitles, F fullscreen, T theater, I mini player,
 * 0–9 a tenth of the way, Home/End, `<`/`>` (or `,`/`.`) speed. Keys with
 * Alt, Ctrl or Meta belong to the browser.
 */
export function playerKeyAction(e: KeyLike): PlayerAction | null {
  if (e.altKey || e.ctrlKey || e.metaKey) return null;
  const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
  switch (key) {
    case " ":
    case "k":
      return { type: "toggle" };
    case "ArrowLeft":
      return { type: "seekBy", seconds: -ARROW_SEEK };
    case "ArrowRight":
      return { type: "seekBy", seconds: ARROW_SEEK };
    case "j":
      return { type: "seekBy", seconds: -JUMP_SEEK };
    case "l":
      return { type: "seekBy", seconds: JUMP_SEEK };
    case "ArrowUp":
      return { type: "volumeBy", delta: VOLUME_STEP };
    case "ArrowDown":
      return { type: "volumeBy", delta: -VOLUME_STEP };
    case "m":
      return { type: "mute" };
    case "c":
      return { type: "captions" };
    case "f":
      return { type: "fullscreen" };
    case "t":
      return { type: "theater" };
    case "i":
      return { type: "miniPlayer" };
    case "Home":
      return { type: "seekTo", fraction: 0 };
    case "End":
      return { type: "seekTo", fraction: 1 };
    case "<":
    case ",":
      return { type: "speed", step: -1 };
    case ">":
    case ".":
      return { type: "speed", step: 1 };
  }
  return /^[0-9]$/.test(key) ? { type: "seekTo", fraction: Number(key) / 10 } : null;
}

/** The next playback speed up or down from rate (a rate off the list snaps to its nearest). */
export function nextSpeed(rate: number, step: 1 | -1): number {
  let at = 0;
  PLAYBACK_SPEEDS.forEach((s, i) => {
    if (Math.abs(s - rate) < Math.abs(PLAYBACK_SPEEDS[at]! - rate)) at = i;
  });
  return PLAYBACK_SPEEDS[Math.min(PLAYBACK_SPEEDS.length - 1, Math.max(0, at + step))]!;
}

/**
 * Where playback starts for a resume point: 0 without one, or when it is in
 * the last few seconds (the viewer finished; start over).
 */
export function resumeAt(startAt: number | undefined, duration?: number): number {
  if (!startAt || !Number.isFinite(startAt) || startAt <= 0) return 0;
  if (duration && duration > 0 && startAt >= duration - Math.max(3, duration * 0.02)) return 0;
  return startAt;
}

/** One cue of a seek sprite: a tile of the sprite image for [start, end). */
export interface SpriteCue {
  start: number;
  end: number;
  url: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

function vttSeconds(s: string): number {
  const parts = s.trim().split(":").map(Number);
  return parts.reduce((acc, p) => acc * 60 + p, 0);
}

/** The cues of a sprite.vtt; tile URLs resolve against the cue sheet's. */
export function parseSpriteVtt(text: string, base: string): SpriteCue[] {
  const cues: SpriteCue[] = [];
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length - 1; i++) {
    const timing = lines[i]!.match(/^\s*([\d:.]+)\s+-->\s+([\d:.]+)/);
    const tile = timing && lines[i + 1]!.trim().match(/^(\S+)#xywh=(\d+),(\d+),(\d+),(\d+)$/);
    if (!timing || !tile) continue;
    const [x, y, w, h] = tile.slice(2).map(Number) as [number, number, number, number];
    cues.push({ start: vttSeconds(timing[1]!), end: vttSeconds(timing[2]!), url: new URL(tile[1]!, base).href, x, y, w, h });
  }
  return cues;
}

/** The cue showing at time t (cues ascend): the last that starts at or before it. */
export function spriteCueAt(cues: readonly SpriteCue[], t: number): SpriteCue | undefined {
  let lo = 0;
  let hi = cues.length - 1;
  let found: SpriteCue | undefined;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (cues[mid]!.start <= t) {
      found = cues[mid];
      lo = mid + 1;
    } else hi = mid - 1;
  }
  return found ?? cues[0];
}

/** A selectable audio or subtitle track. */
export interface PlayerTrack {
  index: number;
  /** The playlist's NAME, else its language. */
  label: string;
  /** BCP 47. */
  lang?: string;
  forced?: boolean;
}

/** The track for a BCP 47 tag: an exact match, else the same primary language; -1 for none. */
export function pickTrack(tracks: readonly { lang?: string }[], lang: string | null | undefined): number {
  if (!lang) return -1;
  const want = lang.toLowerCase();
  const exact = tracks.findIndex((t) => t.lang?.toLowerCase() === want);
  if (exact >= 0) return exact;
  const primary = want.split("-")[0];
  return tracks.findIndex((t) => t.lang?.toLowerCase().split("-")[0] === primary);
}

/** A playhead sample: wall-clock ms and media seconds. */
export interface PlaySample {
  at: number;
  pos: number;
}

/** Seconds actually played between two samples: time that both elapsed and advanced the playhead (a seek adds nothing). */
export function playedBetween(a: PlaySample, b: PlaySample): number {
  return Math.max(0, Math.min((b.at - a.at) / 1000, b.pos - a.pos));
}

export const PLAYER_VOLUME_KEY = "ckui.player.volume";
export const PLAYER_TRACKS_KEY = "ckui.player.tracks";

export interface VolumePrefs {
  volume: number;
  muted: boolean;
}

/** Audio and subtitle languages the viewer chose; subtitles "off" when they turned them off. */
export interface TrackPrefs {
  audio?: string;
  subtitles?: string;
}

function readJSON(key: string | null | undefined): Record<string, unknown> {
  if (!key) return {};
  try {
    const v: unknown = JSON.parse(globalThis.localStorage?.getItem(key) ?? "{}");
    return v && typeof v === "object" ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

function writeJSON(key: string | null | undefined, value: object) {
  if (!key) return;
  try {
    globalThis.localStorage?.setItem(key, JSON.stringify({ ...readJSON(key), ...value }));
  } catch {
    // private mode or blocked storage: the choice lasts this page
  }
}

export function readVolume(key: string | null | undefined): VolumePrefs {
  const v = readJSON(key);
  const volume = typeof v.volume === "number" && Number.isFinite(v.volume) ? Math.min(1, Math.max(0, v.volume)) : 1;
  return { volume, muted: v.muted === true };
}

export const writeVolume = (key: string | null | undefined, prefs: Partial<VolumePrefs>) => writeJSON(key, prefs);

export function readTrackPrefs(key: string | null | undefined): TrackPrefs {
  const v = readJSON(key);
  return { audio: typeof v.audio === "string" ? v.audio : undefined, subtitles: typeof v.subtitles === "string" ? v.subtitles : undefined };
}

export const writeTrackPrefs = (key: string | null | undefined, prefs: TrackPrefs) => writeJSON(key, prefs);

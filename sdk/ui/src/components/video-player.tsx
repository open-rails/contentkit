import {
  AlertCircleIcon,
  ArrowShrink02Icon,
  ClosedCaptionIcon,
  FullScreenIcon,
  PauseIcon,
  PictureInPicture01Icon,
  PictureInPictureExitIcon,
  PictureInPictureOnIcon,
  PlayIcon,
  RectangularIcon,
  Refresh01Icon,
  Settings01Icon,
} from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useEffect, useRef, useState, type CSSProperties, type MouseEvent, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import { formatDuration } from "../client/gallery.js";
import type { PlaybackError } from "../client/playback.js";
import {
  JUMP_SEEK,
  PLAYBACK_SPEEDS,
  PLAYER_VOLUME_KEY,
  nextSpeed,
  playerKeyAction,
  resumeAt,
  writeVolume,
  type PlayerAction,
  type PlayerTrack,
} from "../client/player.js";
import { useHlsPlayer, type HlsPlayerOptions } from "../react/gallery.js";
import { useInlinePreview } from "../react/inline-preview.js";
import { useActiveCues, useFullscreen, useMediaState, usePictureInPicture, useProgressReports, useSprite, type PlaybackProgress } from "../react/player.js";
import { useMessages } from "../i18n/context.js";
import { ContentKitUiRoot } from "../scope.js";
import { RenditionImg } from "./rendition-img.js";
import { publicRenditions, type PublicPreset } from "../client/public.js";
import type { EncodeProgress as Progress } from "../client/generated/wire.js";
import { EncodeProgress } from "./encode-progress.js";
import { Button } from "#ckui/ui/button";
import { Captions, ControlButton, SeekBar, SeekFlash, SettingsMenu, Spinner, VolumeControl, clock, playerButtonClass, type MenuSection, type SeekFlashState } from "./player-controls.js";

type XhrSetup = HlsPlayerOptions["xhrSetup"];

/** The first frame of a video's seek sprite (sprite.vtt), fitted like object-fit. */
export function SpriteFrame({ vtt, xhrSetup, fit = "contain", className }: { vtt: string; xhrSetup?: XhrSetup; fit?: "contain" | "cover"; className?: string }) {
  const sprite = useSprite(vtt, xhrSetup);
  const cue = sprite?.cues[0];
  if (!sprite || !cue) return null;
  return (
    <svg
      aria-hidden
      className={cn("pointer-events-none absolute inset-0 size-full", className)}
      viewBox={`${cue.x} ${cue.y} ${cue.w} ${cue.h}`}
      preserveAspectRatio={fit === "cover" ? "xMidYMid slice" : "xMidYMid meet"}
      data-ckui="sprite-frame"
    >
      <image href={cue.url} width={sprite.width} height={sprite.height} />
    </svg>
  );
}

type At<T> = T extends unknown ? T & { time: number } : never;

/** Playback as it happens, for analytics; never during an inline preview. */
export type PlayerEvent = At<
  | { type: "play" | "playing" | "pause" | "waiting" | "ended" }
  | { type: "seek"; from: number }
  | { type: "rate"; rate: number }
  | { type: "volume"; volume: number; muted: boolean }
  /** The viewer's quality choice; height null is Auto. */
  | { type: "quality"; height: number | null }
  | { type: "audio"; track: PlayerTrack }
  | { type: "subtitles"; track: PlayerTrack | null }
  | { type: "fullscreen" | "pip" | "theater"; on: boolean }
  | { type: "error"; error: PlaybackError }
>;

/** Everything a VideoMiniPlayer needs to carry on where the player stopped. */
export interface PlayerHandoff {
  base: string;
  time: number;
  playing: boolean;
  width?: number;
  height?: number;
  duration?: number;
  poster?: string | PublicPreset | null;
  audioLanguage?: string;
  subtitleLanguage?: string | null;
}

/** A host entry in the settings menu: a choice (e.g. version) or an action (e.g. report a problem). */
export type PlayerMenuItem =
  | { label: string; value: string; options: readonly { value: string; label: string }[]; onChange: (value: string) => void }
  | { label: string; onSelect: () => void };

export interface PlayerSlotContext {
  /** Where popups must portal to stay visible: the player while fullscreen, else undefined (the body). */
  container?: HTMLElement;
  /** The control buttons' class. */
  className: string;
}

export interface VideoPlayerProps extends Omit<HlsPlayerOptions, "src"> {
  /** The ladder's HLS folder (client.media.hlsBase: {media}/{kind}/{id}/hls/{dir}): master.m3u8 and sprite.vtt resolve under it. */
  base?: string | null;
  /** Shown until playback starts: the poster's public preset or a URL; default the first sprite frame. */
  poster?: string | PublicPreset | null;
  /** The video's size (read API w/h): the box is reserved before anything loads; default the decoded frame's. */
  width?: number;
  height?: number;
  duration?: number;
  /** A pending encode (an editor read's `pending` and `progress`): shows its progress instead of a player. */
  pending?: boolean;
  progress?: Progress | null;
  /** Why the video cannot be encoded (an editor read's `failed.message`). */
  failed?: string;
  /**
   * `frame` is as wide as its column allows at the video's aspect, never
   * taller than `maxHeight`; `fill` fills a sized parent (a carousel slide).
   */
  layout?: "frame" | "fill";
  maxHeight?: string;
  /** Preview muted inline before play (hover, or in view on touch); default the provider's. */
  inlinePreview?: boolean;
  /** Where the preview starts, seconds; default 10% in. */
  previewStart?: number;
  /** Shortcuts while focus is in the player (default), page-wide (a watch page: one such player per page), or none. */
  keyboard?: "focus" | "global" | false;
  /** Resume points and watch time: every `progressInterval` of playback, and on pause, seek, end, page hide and unload. */
  onProgress?: (progress: PlaybackProgress) => void;
  /** Milliseconds. Default 10000. */
  progressInterval?: number;
  onEvent?: (event: PlayerEvent) => void;
  /** Theater mode is the host's layout; the button shows with onTheaterChange. */
  theater?: boolean;
  onTheaterChange?: (theater: boolean) => void;
  /** Shows the mini-player button: the player pauses and hands over where it was. */
  onMiniPlayer?: (handoff: PlayerHandoff) => void;
  /** A control-bar slot for the host's downloads. */
  renderDownloads?: (ctx: PlayerSlotContext) => ReactNode;
  /** Host entries in the settings menu, after its own. */
  menuItems?: readonly PlayerMenuItem[];
  crossOrigin?: "anonymous" | "use-credentials";
  label?: string;
  className?: string;
  style?: CSSProperties;
  appearance?: ContentKitUiAppearance;
}

const CONTROLS_IDLE_MS = 2500;
const DOUBLE_TAP_MS = 300;
const TAP_SEEK_CONTINUE_MS = 700;
const SEEK_FLASH_MS = 650;

const TEXT_ENTRY =
  'textarea, select, [contenteditable]:not([contenteditable="false"]), [role=textbox], [role=combobox], [role=searchbox], input:not([type=range]):not([type=button]):not([type=checkbox]):not([type=radio]):not([type=submit]):not([type=reset])';
const MENU = "[role=menu], [role=menuitem], [role=menuitemradio], [role=menuitemcheckbox], [role=listbox], [role=option]";
const OUTSIDE_INTERACTIVE = "a, button, input, [role=button], [role=dialog], [role=alertdialog]";

/**
 * Whether a shortcut key belongs to the player: never while typing or in a
 * menu, not the arrows on a slider (it steps itself), and outside the
 * player (page-wide shortcuts) not on another control.
 */
export function keyAllowed(target: EventTarget | null, key: string, root: Element): boolean {
  if (!(target instanceof Element)) return true;
  if (target.closest(TEXT_ENTRY) || target.closest(MENU)) return false;
  if (target.closest("input[type=range], [role=slider]") && /^(Arrow|Page|Home$|End$)/.test(key)) return false;
  return root.contains(target) || !target.closest(OUTSIDE_INTERACTIVE);
}

// Space on one of the player's buttons is play/pause, not a click on the button last used.
const spaceOnButton = (target: EventTarget | null, root: Element) =>
  target instanceof Element && root.contains(target) && !!target.closest("button") && !target.closest(MENU);

/**
 * An HLS player that never spins forever: poster and play control, its own
 * controls (seek with sprite preview, volume, subtitles, audio, quality,
 * speed, picture-in-picture, theater, fullscreen, mini player), shortcuts,
 * touch gestures, resume and progress reports, and a specific error with
 * Retry and a support code.
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
  inlinePreview,
  previewStart,
  keyboard = "focus",
  onProgress,
  progressInterval = 10_000,
  onEvent,
  theater,
  onTheaterChange,
  onMiniPlayer,
  renderDownloads,
  menuItems,
  crossOrigin,
  label,
  className,
  style,
  appearance,
  startAt,
  ...options
}: VideoPlayerProps) {
  const { t } = useMessages();
  const playable = !!base && !pending && !failed;
  const src = playable ? `${base}master.m3u8` : null;
  const start = resumeAt(startAt, duration);
  const player = useHlsPlayer({ ...options, src, startAt: start });
  const { status, error, started, previewing, audio, subtitles, quality } = player;
  const [root, setRoot] = useState<HTMLDivElement | null>(null);
  const [video, setVideo] = useState<HTMLVideoElement | null>(null);
  const attach = player.ref;
  const videoRef = useCallback(
    (el: HTMLVideoElement | null) => {
      attach(el);
      setVideo(el);
    },
    [attach],
  );
  useInlinePreview({
    setting: inlinePreview,
    available: playable && !started && !error && options.active !== false && !options.autoPlay,
    target: root,
    playing: started && status === "playing",
    start: () => player.preview(previewStartAt(previewStart, duration)),
    stop: player.unload,
  });
  const media = useMediaState(video);
  const fs = useFullscreen(root, video);
  const pip = usePictureInPicture(video);
  const cues = useActiveCues(video);
  const sprite = useSprite(started && base ? `${base}sprite.vtt` : null, options.xhrSetup);
  const live = !previewing && started;
  useProgressReports(video, { enabled: live, source: live ? src : null, interval: progressInterval, onProgress });

  // Events.
  const emit = useRef(onEvent);
  emit.current = onEvent;
  const sending = useRef(false);
  sending.current = !previewing && status !== "idle";
  const send = useCallback((e: { type: string; [k: string]: unknown }, always = false) => {
    if ((always || sending.current) && emit.current) emit.current({ ...e, time: video?.currentTime ?? 0 } as PlayerEvent);
  }, [video]);
  useEffect(() => {
    if (!video) return;
    let from = 0;
    const h: Record<string, () => void> = {
      play: () => send({ type: "play" }),
      playing: () => send({ type: "playing" }),
      pause: () => !video.ended && send({ type: "pause" }),
      waiting: () => send({ type: "waiting" }),
      ended: () => send({ type: "ended" }),
      timeupdate: () => {
        if (!video.seeking) from = video.currentTime;
      },
      seeked: () => {
        send({ type: "seek", from });
        from = video.currentTime;
      },
      ratechange: () => send({ type: "rate", rate: video.playbackRate }),
    };
    for (const [k, f] of Object.entries(h)) video.addEventListener(k, f);
    return () => {
      for (const [k, f] of Object.entries(h)) video.removeEventListener(k, f);
    };
  }, [video, send]);
  useEffect(() => {
    if (error && !previewing) send({ type: "error", error }, true);
  }, [error, previewing, send]);
  const was = useRef({ fs: false, pip: false });
  useEffect(() => {
    if (was.current.fs !== fs.active) send({ type: "fullscreen", on: fs.active });
    if (was.current.pip !== pip.active) send({ type: "pip", on: pip.active });
    was.current = { fs: fs.active, pip: pip.active };
  }, [fs.active, pip.active, send]);

  // A new resume point for the playing source seeks to it.
  const lastStart = useRef(start);
  useEffect(() => {
    if (lastStart.current === start) return;
    lastStart.current = start;
    if (video && started) video.currentTime = start;
  }, [start, video, started]);

  // Controls show on pointer movement and hide after a pause in it while playing.
  const [poked, setPoked] = useState(false);
  const idle = useRef<ReturnType<typeof setTimeout>>(undefined);
  const poke = useCallback(() => {
    setPoked(true);
    clearTimeout(idle.current);
    idle.current = setTimeout(() => setPoked(false), CONTROLS_IDLE_MS);
  }, []);
  const hide = useCallback(() => {
    clearTimeout(idle.current);
    setPoked(false);
  }, []);
  useEffect(() => () => clearTimeout(idle.current), []);
  useEffect(() => {
    if (started) poke();
  }, [started, poke]);
  const [menuOpen, setMenuOpen] = useState(false);
  const [scrubbing, setScrubbing] = useState(false);
  const [keyFocus, setKeyFocus] = useState(false);
  const controlsShown = started && !error && (poked || media.paused || media.ended || menuOpen || scrubbing || keyFocus);

  const [flash, setFlash] = useState<SeekFlashState | null>(null);
  const flashTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const flashSeek = useCallback((direction: -1 | 1, seconds: number) => {
    const continuing = flashTimer.current !== undefined;
    clearTimeout(flashTimer.current);
    setFlash((prev) => ({ direction, seconds: continuing && prev?.direction === direction ? prev.seconds + seconds : seconds }));
    flashTimer.current = setTimeout(() => {
      setFlash(null);
      flashTimer.current = undefined;
    }, SEEK_FLASH_MS);
  }, []);
  useEffect(() => () => clearTimeout(flashTimer.current), []);

  const volumeKey = options.volumeKey === undefined ? PLAYER_VOLUME_KEY : options.volumeKey;
  const dur = media.duration || duration || 0;
  const seek = (to: number) => {
    if (video) video.currentTime = Math.min(dur || to, Math.max(0, to));
  };
  const seekBy = (seconds: number) => {
    if (!video) return;
    seek(video.currentTime + seconds);
    flashSeek(seconds < 0 ? -1 : 1, Math.abs(seconds));
  };
  const toggle = () => {
    if (!video) return;
    if (!started || video.ended) {
      if (video.ended) video.currentTime = 0;
      player.play();
    } else if (video.paused) player.play();
    else video.pause();
  };
  const setVolume = (v: number) => {
    if (!video) return;
    video.volume = Math.min(1, Math.max(0, v));
    if (video.volume > 0 && video.muted) video.muted = false;
    writeVolume(volumeKey, { volume: video.volume, muted: video.muted });
    send({ type: "volume", volume: video.volume, muted: video.muted });
  };
  const toggleMute = () => {
    if (!video) return;
    video.muted = !video.muted;
    if (!video.muted && video.volume === 0) video.volume = 1;
    writeVolume(volumeKey, { volume: video.volume, muted: video.muted });
    send({ type: "volume", volume: video.volume, muted: video.muted });
  };
  const selectAudio = (i: number) => {
    audio.select(i);
    const track = audio.list[i];
    if (track) send({ type: "audio", track });
  };
  const lastSub = useRef(-1);
  if (subtitles.selected >= 0) lastSub.current = subtitles.selected;
  const selectSubs = (i: number) => {
    subtitles.select(i);
    send({ type: "subtitles", track: subtitles.list[i] ?? null });
  };
  const toggleCaptions = () => selectSubs(subtitles.selected >= 0 ? -1 : lastSub.current >= 0 ? lastSub.current : 0);
  const selectQuality = (i: number) => {
    quality.select(i);
    send({ type: "quality", height: quality.levels.find((l) => l.index === i)?.height ?? null });
  };
  const toggleTheater = () => {
    onTheaterChange?.(!theater);
    send({ type: "theater", on: !theater });
  };
  const openMini = () => {
    if (!onMiniPlayer || !base || !video) return;
    const a = audio.list[audio.selected];
    const s = subtitles.list[subtitles.selected];
    const handoff: PlayerHandoff = {
      base,
      time: video.currentTime,
      playing: !video.paused && !video.ended,
      width: width ?? (video.videoWidth || undefined),
      height: height ?? (video.videoHeight || undefined),
      duration: dur || undefined,
      poster,
      audioLanguage: a?.lang,
      subtitleLanguage: s?.lang ?? null,
    };
    video.pause();
    if (fs.active) fs.toggle();
    onMiniPlayer(handoff);
  };

  const run = (a: PlayerAction): boolean => {
    if (!video) return false;
    if (!started && a.type !== "toggle") return false;
    switch (a.type) {
      case "toggle":
        toggle();
        break;
      case "seekBy":
        seekBy(a.seconds);
        break;
      case "seekTo":
        if (!dur) return false;
        seek(dur * a.fraction);
        break;
      case "volumeBy":
        setVolume((video.muted ? 0 : video.volume) + a.delta);
        break;
      case "mute":
        toggleMute();
        break;
      case "captions":
        if (!subtitles.list.length) return false;
        toggleCaptions();
        break;
      case "fullscreen":
        if (!fs.supported) return false;
        fs.toggle();
        break;
      case "theater":
        if (!onTheaterChange) return false;
        toggleTheater();
        break;
      case "miniPlayer":
        if (!onMiniPlayer) return false;
        openMini();
        break;
      case "speed":
        video.playbackRate = nextSpeed(video.playbackRate, a.step);
        break;
    }
    if (started) poke();
    return true;
  };
  const runRef = useRef(run);
  runRef.current = run;
  const active = options.active !== false;
  useEffect(() => {
    if (!keyboard || !root || !active) return;
    const on: EventTarget = keyboard === "global" ? document : root;
    const down = (e: Event) => {
      const k = e as KeyboardEvent;
      if (k.defaultPrevented) return;
      const action = playerKeyAction(k);
      if (!action || !keyAllowed(k.target, k.key, root) || !runRef.current(action)) return;
      k.preventDefault();
    };
    const up = (e: Event) => {
      const k = e as KeyboardEvent;
      if (k.key === " " && spaceOnButton(k.target, root)) k.preventDefault();
    };
    on.addEventListener("keydown", down);
    on.addEventListener("keyup", up);
    return () => {
      on.removeEventListener("keydown", down);
      on.removeEventListener("keyup", up);
    };
  }, [keyboard, root, active]);

  // Mouse: click plays and pauses, double click is fullscreen. Touch: a tap
  // shows or hides the controls; double taps on either side seek 10 s, and
  // taps that keep coming keep seeking.
  const pointerType = useRef("mouse");
  const lastTap = useRef<{ at: number; side: -1 | 1 } | null>(null);
  const tapSeekUntil = useRef(0);
  const onSurfaceClick = (e: MouseEvent<HTMLDivElement>) => {
    if (pointerType.current === "mouse") {
      toggle();
      poke();
      return;
    }
    const r = e.currentTarget.getBoundingClientRect();
    const x = r.width ? (e.clientX - r.left) / r.width : 0.5;
    const side = x < 0.4 ? -1 : x > 0.6 ? 1 : null;
    const now = Date.now();
    const last = lastTap.current;
    lastTap.current = side ? { at: now, side } : null;
    if (side && last?.side === side && (now < tapSeekUntil.current || now - last.at <= DOUBLE_TAP_MS)) {
      tapSeekUntil.current = now + TAP_SEEK_CONTINUE_MS;
      seekBy(side * JUMP_SEEK);
      return;
    }
    if (controlsShown && !media.paused) hide();
    else poke();
  };
  const onSurfaceDoubleClick = () => {
    if (Date.now() >= tapSeekUntil.current && fs.supported) fs.toggle();
  };

  const aspect = width && height ? width / height : (media.aspect ?? 16 / 9);
  const frame: CSSProperties = fs.active
    ? { width: "100%", height: "100%", borderRadius: 0 }
    : layout === "frame"
      ? { aspectRatio: String(aspect), width: `min(100%, calc(${maxHeight} * ${aspect}))`, marginInline: "auto" }
      : {};
  const posterOutputs = typeof poster === "string" ? [] : publicRenditions(poster);
  const busy = !error && !previewing && (status === "loading" || status === "buffering");
  // The preview shows once frames play; the cover stays until then.
  const shown = previewing && status === "playing";
  const fade = cn("transition-opacity duration-300 motion-reduce:transition-none", shown && "opacity-0");
  const portal = fs.active && root ? root : undefined;
  const sections = settingsSections();

  function trackName(tr: PlayerTrack) {
    const name = tr.label || t("player.track", { number: tr.index + 1 });
    return tr.forced ? t("player.forced", { label: name }) : name;
  }
  function settingsSections(): MenuSection[] {
    const out: MenuSection[] = [];
    if (audio.list.length > 1)
      out.push({ key: "audio", label: t("player.audio"), value: String(audio.selected), options: audio.list.map((x) => ({ value: String(x.index), label: trackName(x) })), onChange: (v) => selectAudio(Number(v)) });
    if (subtitles.list.length)
      out.push({
        key: "subtitles",
        label: t("player.subtitles"),
        value: String(subtitles.selected),
        options: [{ value: "-1", label: t("player.subtitlesOff") }, ...subtitles.list.map((x) => ({ value: String(x.index), label: trackName(x) }))],
        onChange: (v) => selectSubs(Number(v)),
      });
    if (quality.levels.length > 1) {
      const playing = quality.levels.find((l) => l.index === quality.current);
      out.push({
        key: "quality",
        label: t("player.quality"),
        value: String(quality.selected),
        options: [
          ...[...quality.levels].reverse().map((l) => ({ value: String(l.index), label: qualityLabel(l.height) })),
          { value: "-1", label: playing && quality.selected === -1 ? t("player.autoCurrent", { quality: qualityLabel(playing.height).split(" ")[0]! }) : t("player.auto") },
        ],
        onChange: (v) => selectQuality(Number(v)),
      });
    }
    out.push({
      key: "speed",
      label: t("player.speed"),
      value: String(media.rate),
      options: PLAYBACK_SPEEDS.map((r) => ({ value: String(r), label: r === 1 ? t("player.normal") : `${r}×` })),
      onChange: (v) => {
        if (video) video.playbackRate = Number(v);
      },
    });
    menuItems?.forEach((m, i) => {
      if ("options" in m) out.push({ key: `host-${i}`, label: m.label, value: m.value, options: m.options, onChange: m.onChange });
    });
    return out;
  }
  const actions = menuItems?.filter((m): m is Extract<PlayerMenuItem, { onSelect: () => void }> => "onSelect" in m);

  return (
    <ContentKitUiRoot
      appearance={appearance}
      className={cn(
        "@container relative w-full touch-manipulation overflow-hidden bg-black text-white outline-none",
        layout === "fill" ? "size-full" : !fs.active && "rounded-lg",
        started && !controlsShown && "cursor-none",
        className,
      )}
      style={{ ...frame, ...style }}
      data-ckui="video-player"
      data-status={failed ? "failed" : pending ? "pending" : status}
      data-previewing={previewing ? "" : undefined}
      data-fullscreen={fs.active ? "" : undefined}
      data-controls={controlsShown ? "" : undefined}
      ref={setRoot}
      role="group"
      aria-label={label ?? t("player.label")}
      tabIndex={playable ? -1 : undefined}
      onPointerDown={(e) => (pointerType.current = e.pointerType || "mouse")}
      onPointerMove={(e) => e.pointerType === "mouse" && started && poke()}
      onPointerLeave={(e) => e.pointerType === "mouse" && !scrubbing && !menuOpen && hide()}
      onFocus={(e) => setKeyFocus(focusVisible(e.target))}
      onBlur={() => setKeyFocus(false)}
    >
      {failed ? (
        <Panel icon>{t("player.failed")}</Panel>
      ) : pending || !base ? (
        <div className="absolute inset-0 flex items-center justify-center bg-muted p-6 text-foreground">
          <EncodeProgress progress={progress} className="max-w-xs" />
        </div>
      ) : (
        <>
          <video ref={videoRef} className="absolute inset-0 size-full object-contain" playsInline preload="none" crossOrigin={crossOrigin} aria-label={label} />
          {started && !error && <div data-ckui="surface" className="absolute inset-0" onClick={onSurfaceClick} onDoubleClick={onSurfaceDoubleClick} />}
          {!started && !error && (
            <>
              {typeof poster === "string" ? (
                <img alt="" src={poster} decoding="async" className={cn("pointer-events-none absolute inset-0 size-full object-contain", fade)} />
              ) : posterOutputs.length ? (
                <RenditionImg outputs={posterOutputs} className={cn("pointer-events-none absolute inset-0 size-full object-contain", fade)} />
              ) : (
                <SpriteFrame vtt={`${base}sprite.vtt`} xhrSetup={options.xhrSetup} className={fade} />
              )}
              <BigButton label={t("player.play")} onClick={player.play} disabled={busy} className={fade}>
                {busy ? <Spinner /> : <HugeiconsIcon icon={PlayIcon} className="ml-1 size-7 fill-current" strokeWidth={1.5} />}
              </BigButton>
              {duration ? (
                <span className="pointer-events-none absolute right-2 bottom-2 rounded bg-black/65 px-1.5 py-0.5 text-xs font-medium tabular-nums">{formatDuration(duration)}</span>
              ) : null}
            </>
          )}
          {started && !error && media.ended && (
            <BigButton label={t("player.replay")} onClick={toggle}>
              <HugeiconsIcon icon={Refresh01Icon} className="size-7" strokeWidth={1.75} />
            </BigButton>
          )}
          <Captions cues={live ? cues : []} bottom={controlsShown ? "4.75rem" : "1.25rem"} />
          {started && busy && (
            <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
              <span className="flex size-14 items-center justify-center rounded-full bg-black/45">
                <Spinner />
              </span>
            </div>
          )}
          <SeekFlash flash={flash} />
          {started && !error && (
            <div
              data-ckui="controls"
              data-ckui-noswipe=""
              className={cn(
                "pointer-events-none absolute inset-x-0 bottom-0 z-10 flex flex-col bg-linear-to-t from-black/75 via-black/35 to-transparent px-2 pt-12 pb-1 transition-opacity duration-200 motion-reduce:transition-none",
                controlsShown ? "opacity-100 *:pointer-events-auto" : "opacity-0",
              )}
            >
              <SeekBar time={media.time} duration={dur} buffered={media.buffered} onSeek={seek} onScrub={(v) => setScrubbing(v !== null)} sprite={sprite} />
              <div className="flex items-center gap-0.5">
                <ControlButton
                  label={media.ended ? t("player.replay") : media.paused ? t("player.resume") : t("player.pause")}
                  shortcut="k"
                  icon={media.ended ? Refresh01Icon : media.paused ? PlayIcon : PauseIcon}
                  onClick={toggle}
                  data-ckui="play"
                />
                <VolumeControl volume={media.volume} muted={media.muted} onVolume={setVolume} onMute={toggleMute} />
                <span className="ml-1 shrink-0 text-xs font-medium whitespace-nowrap text-white/90 tabular-nums" data-ckui="time">
                  {clock(media.time)} / {clock(dur)}
                </span>
                <div className="min-w-0 flex-1" />
                {subtitles.list.length > 0 && (
                  <ControlButton label={t("player.subtitles")} shortcut="c" icon={ClosedCaptionIcon} aria-pressed={subtitles.selected >= 0} onClick={toggleCaptions} data-ckui="captions-toggle" />
                )}
                {renderDownloads?.({ container: portal, className: playerButtonClass })}
                <SettingsMenu icon={Settings01Icon} label={t("player.settings")} sections={sections} actions={actions} container={portal} onOpenChange={setMenuOpen} />
                {onMiniPlayer && <ControlButton className="@max-sm:hidden" label={t("player.miniPlayer")} shortcut="i" icon={PictureInPictureOnIcon} onClick={openMini} data-ckui="mini-player-open" />}
                {pip.supported && (
                  <ControlButton
                    className="@max-md:hidden"
                    label={pip.active ? t("player.exitPip") : t("player.pip")}
                    icon={pip.active ? PictureInPictureExitIcon : PictureInPicture01Icon}
                    onClick={pip.toggle}
                    data-ckui="pip"
                  />
                )}
                {onTheaterChange && (
                  <ControlButton
                    className="max-lg:hidden"
                    label={t("player.theater")}
                    shortcut="t"
                    icon={RectangularIcon}
                    aria-pressed={!!theater}
                    onClick={toggleTheater}
                    data-ckui="theater"
                  />
                )}
                {fs.supported && (
                  <ControlButton
                    label={fs.active ? t("player.exitFullscreen") : t("player.fullscreen")}
                    shortcut="f"
                    icon={fs.active ? ArrowShrink02Icon : FullScreenIcon}
                    onClick={fs.toggle}
                    data-ckui="fullscreen"
                  />
                )}
              </div>
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
    </ContentKitUiRoot>
  );
}

function focusVisible(el: EventTarget) {
  try {
    return el instanceof Element && el.matches(":focus-visible") && !!el.closest("[data-ckui=controls]");
  } catch {
    return false;
  }
}

function BigButton({ label, onClick, disabled, className, children }: { label: string; onClick: () => void; disabled?: boolean; className?: string; children: ReactNode }) {
  return (
    <button
      type="button"
      className="absolute inset-0 z-[7] flex items-center justify-center outline-none focus-visible:[&>span]:ring-4 focus-visible:[&>span]:ring-white/60"
      onClick={onClick}
      aria-label={label}
      disabled={disabled}
    >
      <span className={cn("flex size-16 items-center justify-center rounded-full bg-black/55 shadow-lg backdrop-blur-sm transition motion-safe:hover:scale-105", className)}>{children}</span>
    </button>
  );
}

/** A preview's start: the given second, else 10% in, kept a second before the end. */
export function previewStartAt(start: number | undefined, duration: number | undefined): number {
  const d = duration ?? 0;
  const t = start ?? d * 0.1;
  return Math.max(0, d > 1 ? Math.min(t, d - 1) : 0);
}

/** "2160p 4K", "1080p HD", "720p": a rung by its short side. */
export function qualityLabel(height: number): string {
  return `${height}p${height >= 2160 ? " 4K" : height === 1080 ? " HD" : ""}`;
}

function Panel({ children, icon, code, onRetry }: { children: string; icon?: boolean; code?: string; onRetry?: () => void }) {
  const { t } = useMessages();
  return (
    <div role="alert" data-ckui="player-error" data-ckui-noswipe="" className="absolute inset-0 z-20 flex flex-col items-center justify-center gap-3 bg-black/80 p-6 text-center">
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

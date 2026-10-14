import { Cancel01Icon, GoBackward10SecIcon, GoForward10SecIcon, SquareArrowExpand01Icon, PauseIcon, PlayIcon, VolumeHighIcon, VolumeOffIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useEffect, useRef, useState, type CSSProperties, type MouseEvent } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import { JUMP_SEEK, PLAYER_VOLUME_KEY, playerKeyAction, writeVolume, type PlayerAction } from "../client/player.js";
import { publicRenditions } from "../client/public.js";
import { useMessages } from "../i18n/context.js";
import { useHlsPlayer, type HlsPlayerOptions } from "../react/gallery.js";
import { useActiveCues, useMediaState, useProgressReports, type PlaybackProgress } from "../react/player.js";
import { ContentKitUiRoot } from "../scope.js";
import { Captions, ControlButton, SeekBar, Spinner, clock } from "./player-controls.js";
import { RenditionImg } from "./rendition-img.js";
import { SpriteFrame, keyAllowed, type PlayerHandoff } from "./video-player.js";

const CONTROLS_IDLE_MS = 2500;

export interface VideoMiniPlayerProps extends Omit<HlsPlayerOptions, "src" | "startAt" | "autoPlay" | "audioLanguage" | "subtitleLanguage" | "active"> {
  /** What VideoPlayer's onMiniPlayer handed over. */
  handoff: PlayerHandoff;
  /** Back to the full player, where this one is. */
  onExpand: (state: { time: number; playing: boolean }) => void;
  onClose: (state: { time: number }) => void;
  onProgress?: (progress: PlaybackProgress) => void;
  /** Milliseconds. Default 10000. */
  progressInterval?: number;
  /** K, J, L and M page-wide (default), every shortcut while focus is in it, or none. */
  keyboard?: "global" | "focus" | false;
  /** The video's title. */
  label?: string;
  position?: "bottom-right" | "bottom-left";
  crossOrigin?: "anonymous" | "use-credentials";
  className?: string;
  style?: CSSProperties;
  appearance?: ContentKitUiAppearance;
}

/**
 * The video docked in a corner while the viewer browses: play, ±10 s, mute,
 * a seek line, expand back to the full player and close. It starts where
 * the handoff stopped, playing if it was.
 */
export function VideoMiniPlayer({
  handoff,
  onExpand,
  onClose,
  onProgress,
  progressInterval = 10_000,
  keyboard = "global",
  label,
  position = "bottom-right",
  crossOrigin,
  className,
  style,
  appearance,
  ...options
}: VideoMiniPlayerProps) {
  const { t } = useMessages();
  const src = `${handoff.base}master.m3u8`;
  const player = useHlsPlayer({
    ...options,
    src,
    startAt: handoff.time,
    autoPlay: handoff.playing,
    audioLanguage: handoff.audioLanguage,
    subtitleLanguage: handoff.subtitleLanguage,
    // The handoff carries the viewer's choices; a few hundred pixels need no manual rung.
    trackKey: null,
    qualityKey: null,
  });
  const { status, error, started } = player;
  const [video, setVideo] = useState<HTMLVideoElement | null>(null);
  const attach = player.ref;
  const videoRef = useCallback(
    (el: HTMLVideoElement | null) => {
      attach(el);
      setVideo(el);
    },
    [attach],
  );
  const media = useMediaState(video);
  const cues = useActiveCues(video);
  useProgressReports(video, { enabled: started, source: started ? src : null, interval: progressInterval, onProgress });

  const [poked, setPoked] = useState(true);
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
  const [scrubbing, setScrubbing] = useState(false);
  const playing = !media.paused && !media.ended;
  const shown = poked || !playing || scrubbing || !!error;

  const done = useRef(false);
  const time = () => video?.currentTime || media.time || handoff.time;
  const expand = () => {
    if (done.current) return;
    done.current = true;
    onExpand({ time: time(), playing });
  };
  const close = () => {
    if (done.current) return;
    done.current = true;
    video?.pause();
    onClose({ time: time() });
  };
  const toggle = () => {
    if (!video) return;
    if (!started || video.paused || video.ended) player.play();
    else video.pause();
  };
  const dur = media.duration || handoff.duration || 0;
  const seek = (to: number) => {
    if (video) video.currentTime = Math.min(dur || to, Math.max(0, to));
  };
  const volumeKey = options.volumeKey === undefined ? PLAYER_VOLUME_KEY : options.volumeKey;
  const toggleMute = () => {
    if (!video) return;
    video.muted = !video.muted;
    if (!video.muted && video.volume === 0) video.volume = 1;
    writeVolume(volumeKey, { volume: video.volume, muted: video.muted });
  };

  const run = (a: PlayerAction, key: string): boolean => {
    if (!video) return false;
    // Page-wide, only K, J, L and M: Space and the arrows scroll the page.
    if (keyboard === "global" && !"kjlmKJLM".includes(key)) return false;
    switch (a.type) {
      case "toggle":
        toggle();
        break;
      case "seekBy":
        if (!started) return false;
        seek(video.currentTime + a.seconds);
        break;
      case "mute":
        toggleMute();
        break;
      default:
        return false;
    }
    poke();
    return true;
  };
  const runRef = useRef(run);
  runRef.current = run;
  const [root, setRoot] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!keyboard || !root) return;
    const on: EventTarget = keyboard === "global" ? document : root;
    const down = (e: Event) => {
      const k = e as KeyboardEvent;
      if (k.defaultPrevented) return;
      const action = playerKeyAction(k);
      if (!action || !keyAllowed(k.target, k.key, root) || !runRef.current(action, k.key)) return;
      k.preventDefault();
    };
    on.addEventListener("keydown", down);
    return () => on.removeEventListener("keydown", down);
  }, [keyboard, root]);

  // Touch has no hover: a tap shows or hides the controls. A click plays and pauses.
  const pointerType = useRef("mouse");
  const onSurfaceClick = (e: MouseEvent) => {
    e.preventDefault();
    if (pointerType.current === "mouse") {
      toggle();
      poke();
    } else if (shown && playing) hide();
    else poke();
  };

  const aspect = handoff.width && handoff.height ? handoff.width / handoff.height : (media.aspect ?? 16 / 9);
  const posterOutputs = typeof handoff.poster === "string" ? [] : publicRenditions(handoff.poster);
  const busy = !error && (status === "loading" || status === "buffering");
  const overlay = "rounded-full text-white hover:bg-white/15";
  return (
    <ContentKitUiRoot
      ref={setRoot}
      appearance={appearance}
      role="region"
      aria-label={label ? `${t("player.miniPlayer")}: ${label}` : t("player.miniPlayer")}
      data-ckui="mini-player"
      data-status={status}
      className={cn(
        "@container fixed bottom-4 z-50 touch-manipulation overflow-hidden rounded-md bg-black text-white shadow-lg select-none",
        position === "bottom-left" ? "left-4" : "right-4",
        className,
      )}
      style={{ aspectRatio: String(aspect), width: `min(24rem, calc(100vw - 2rem), calc(60svh * ${aspect}))`, ...style }}
      onPointerDown={(e) => (pointerType.current = e.pointerType || "mouse")}
      onPointerMove={(e) => e.pointerType === "mouse" && poke()}
      onPointerLeave={(e) => e.pointerType === "mouse" && !scrubbing && playing && hide()}
    >
      <video ref={videoRef} className="absolute inset-0 size-full object-contain" playsInline preload="none" crossOrigin={crossOrigin} aria-label={label} />
      {!started &&
        (typeof handoff.poster === "string" ? (
          <img alt="" src={handoff.poster} className="pointer-events-none absolute inset-0 size-full object-contain" />
        ) : posterOutputs.length ? (
          <RenditionImg outputs={posterOutputs} className="pointer-events-none absolute inset-0 size-full object-contain" />
        ) : (
          <SpriteFrame vtt={`${handoff.base}sprite.vtt`} xhrSetup={options.xhrSetup} />
        ))}
      <div aria-hidden data-ckui="surface" className="absolute inset-0" onClick={onSurfaceClick} />
      <Captions cues={started ? cues : []} bottom={shown ? "2.75rem" : "0.75rem"} />
      <div
        data-ckui="controls"
        className={cn(
          "pointer-events-none absolute inset-0 bg-black/45 transition-opacity duration-200 focus-within:opacity-100 motion-reduce:transition-none",
          shown ? "opacity-100 [&_button]:pointer-events-auto" : "opacity-0",
        )}
      >
        <div className="absolute inset-x-0 top-0 flex items-center justify-between p-1.5">
          <ControlButton label={t("player.expand")} icon={SquareArrowExpand01Icon} onClick={expand} className={overlay} data-ckui="mini-expand" />
          <ControlButton label={t("player.close")} icon={Cancel01Icon} onClick={close} className={overlay} data-ckui="mini-close" />
        </div>
        <div className="absolute inset-0 m-auto flex h-fit w-fit items-center gap-3 @sm:gap-5">
          <ControlButton
            label={t("player.seekBack", { seconds: JUMP_SEEK })}
            icon={GoBackward10SecIcon}
            disabled={!started}
            onClick={() => {
              seek(time() - JUMP_SEEK);
              poke();
            }}
            className={cn(overlay, "[&_svg]:size-6")}
          />
          <button
            type="button"
            aria-label={playing ? t("player.pause") : t("player.resume")}
            title={playing ? t("player.pause") : t("player.resume")}
            onClick={() => {
              toggle();
              poke();
            }}
            className={cn("flex size-12 cursor-pointer items-center justify-center outline-none focus-visible:ring-2 focus-visible:ring-white/80", overlay)}
            data-ckui="mini-play"
          >
            {busy && started ? <Spinner className="size-7" /> : <HugeiconsIcon icon={playing ? PauseIcon : PlayIcon} className="size-7 fill-current" strokeWidth={1.5} aria-hidden />}
          </button>
          <ControlButton
            label={t("player.seekForward", { seconds: JUMP_SEEK })}
            icon={GoForward10SecIcon}
            disabled={!started}
            onClick={() => {
              seek(time() + JUMP_SEEK);
              poke();
            }}
            className={cn(overlay, "[&_svg]:size-6")}
          />
        </div>
        <div className="absolute inset-x-0 bottom-3 flex items-center justify-between px-2.5">
          <span className="text-xs font-medium tabular-nums">{dur > 0 ? `${clock(media.time || handoff.time)} / ${clock(dur)}` : clock(media.time || handoff.time)}</span>
          <ControlButton label={media.muted ? t("player.unmute") : t("player.mute")} icon={media.muted ? VolumeOffIcon : VolumeHighIcon} onClick={toggleMute} className={cn(overlay, "size-8")} />
        </div>
      </div>
      <div className="absolute inset-x-0 bottom-0 z-10">
        <SeekBar thin time={media.time || handoff.time} duration={dur} buffered={media.buffered} onSeek={seek} onScrub={(v) => setScrubbing(v !== null)} />
      </div>
      {error && (
        <div role="alert" className="absolute inset-0 z-20 flex flex-col items-center justify-center gap-2 bg-black/80 p-3 text-center text-xs">
          <p>{t(`player.errors.${error.kind}`)}</p>
          <button type="button" onClick={player.retry} className="rounded-md bg-white/15 px-2 py-1 font-medium hover:bg-white/25">
            {t("player.retry")}
          </button>
        </div>
      )}
    </ContentKitUiRoot>
  );
}

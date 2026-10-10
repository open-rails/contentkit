import { ArrowLeft01Icon, ArrowRight01Icon, VolumeHighIcon, VolumeLowIcon, VolumeOffIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon, type IconSvgElement } from "@hugeicons/react";
import { Slider as SliderPrimitive } from "@base-ui/react/slider";
import { cn } from "cn";
import { useContext, useLayoutEffect, useRef, useState, type ComponentProps, type KeyboardEvent, type PointerEvent } from "react";
import { formatDuration } from "../client/gallery.js";
import { ARROW_SEEK, spriteCueAt } from "../client/player.js";
import { useMessages } from "../i18n/context.js";
import type { Sprite } from "../react/player.js";
import { AppearanceContext } from "../scope.js";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "#ckui/ui/dropdown-menu";

/** The class of every control-bar button; a host's slotted buttons can share it. */
export const playerButtonClass =
  "inline-flex size-9 shrink-0 cursor-pointer items-center justify-center rounded-full text-white outline-none transition-colors hover:bg-white/15 focus-visible:ring-2 focus-visible:ring-white/80 motion-reduce:transition-none data-popup-open:bg-white/15 [&_svg]:size-5 relative aria-pressed:after:absolute aria-pressed:after:bottom-1 aria-pressed:after:h-0.5 aria-pressed:after:w-4 aria-pressed:after:rounded-full aria-pressed:after:bg-player-accent";

export const clock = (s: number) => formatDuration(Math.floor(s));

export function ControlButton({
  label,
  shortcut,
  icon,
  className,
  ...props
}: Omit<ComponentProps<"button">, "children"> & { label: string; shortcut?: string; icon: IconSvgElement }) {
  return (
    <button type="button" aria-label={label} title={shortcut ? `${label} (${shortcut})` : label} className={cn(playerButtonClass, className)} {...props}>
      <HugeiconsIcon icon={icon} strokeWidth={1.75} aria-hidden />
    </button>
  );
}

const SEEK_KEYS: Record<string, (time: number, duration: number) => number> = {
  ArrowLeft: (t) => t - ARROW_SEEK,
  ArrowDown: (t) => t - ARROW_SEEK,
  ArrowRight: (t) => t + ARROW_SEEK,
  ArrowUp: (t) => t + ARROW_SEEK,
  PageDown: (t, d) => t - d / 10,
  PageUp: (t, d) => t + d / 10,
  Home: () => 0,
  End: (_, d) => d,
};

export interface SeekBarProps {
  time: number;
  duration: number;
  buffered?: number;
  onSeek: (time: number) => void;
  /** The position being dragged to, or null when the drag ends. */
  onScrub?: (time: number | null) => void;
  sprite?: Sprite | null;
  /** The mini player's hairline along its bottom edge. */
  thin?: boolean;
  className?: string;
}

/** The seek slider: buffered range, drag to seek on release, keys in 5 s steps, a sprite and time preview on hover. */
export function SeekBar({ time, duration, buffered = 0, onSeek, onScrub, sprite, thin, className }: SeekBarProps) {
  const { t } = useMessages();
  const [scrub, setScrub] = useState<number | null>(null);
  const [hover, setHover] = useState<{ time: number; x: number; width: number } | null>(null);
  const track = useRef<HTMLDivElement>(null);
  const value = scrub ?? Math.min(time, duration || time);
  const pct = (v: number) => (duration > 0 ? `${Math.min(100, (v / duration) * 100)}%` : "0%");
  const at = (clientX: number) => {
    const r = track.current?.getBoundingClientRect();
    if (!r || !r.width || !duration) return null;
    const x = Math.min(r.width, Math.max(0, clientX - r.left));
    return { time: (x / r.width) * duration, x, width: r.width };
  };
  const scrubTo = (v: number | null) => {
    setScrub(v);
    onScrub?.(v);
  };
  const onKeyDownCapture = (e: KeyboardEvent) => {
    const to = SEEK_KEYS[e.key];
    if (!to || !duration) return;
    // The slider's own steps are tiny; these are the player's.
    e.preventDefault();
    e.stopPropagation();
    onSeek(Math.min(duration, Math.max(0, to(time, duration))));
  };
  const preview = scrub !== null ? { time: scrub, x: (scrub / (duration || 1)) * (track.current?.clientWidth ?? 0), width: track.current?.clientWidth ?? 0 } : hover;
  return (
    <SliderPrimitive.Root
      value={value}
      min={0}
      max={duration || 1}
      step={0.01}
      disabled={!duration}
      thumbAlignment="center"
      onValueChange={(v) => scrubTo(v)}
      onValueCommitted={(v) => {
        scrubTo(null);
        onSeek(v);
      }}
      onKeyDownCapture={onKeyDownCapture}
      className={cn("relative w-full", className)}
      data-ckui="seek"
    >
      <SliderPrimitive.Control
        className={cn("group/seek relative flex w-full cursor-pointer touch-none items-center select-none", thin ? "h-3 items-end" : "h-4")}
        onPointerMove={(e: PointerEvent) => e.pointerType === "mouse" && setHover(at(e.clientX))}
        onPointerLeave={() => setHover(null)}
      >
        <SliderPrimitive.Track
          ref={track}
          className={cn(
            "relative w-full overflow-hidden bg-white/30 transition-[height] motion-reduce:transition-none",
            thin ? "h-[3px] group-hover/seek:h-[5px]" : "h-1 rounded-full group-hover/seek:h-1.5",
            scrub !== null && (thin ? "h-[5px]" : "h-1.5"),
          )}
        >
          <div aria-hidden className="absolute inset-y-0 left-0 bg-white/40" style={{ width: pct(buffered) }} />
          <SliderPrimitive.Indicator className="bg-player-accent" />
        </SliderPrimitive.Track>
        <SliderPrimitive.Thumb
          aria-label={t("player.seek")}
          getAriaValueText={(_, v) => t("player.time", { current: clock(v), duration: clock(duration) })}
          className={cn(
            "size-3 rounded-full bg-player-accent opacity-0 shadow transition-opacity outline-none group-hover/seek:opacity-100 focus-visible:opacity-100 focus-visible:ring-2 focus-visible:ring-white/80 motion-reduce:transition-none",
            scrub !== null && "opacity-100",
            thin && "mb-[-4px]",
          )}
        />
      </SliderPrimitive.Control>
      {preview && !thin && <SeekPreview sprite={sprite} {...preview} />}
    </SliderPrimitive.Root>
  );
}

function SeekPreview({ sprite, time, x, width }: { sprite?: Sprite | null; time: number; x: number; width: number }) {
  const cue = sprite ? spriteCueAt(sprite.cues, time) : undefined;
  const w = cue ? Math.min(160, Math.max(96, width * 0.3)) : 0;
  const s = cue ? w / cue.w : 0;
  const boxW = Math.max(w, 48);
  const left = Math.min(Math.max(0, x - boxW / 2), Math.max(0, width - boxW));
  return (
    <div aria-hidden data-ckui="seek-preview" className="pointer-events-none absolute bottom-full mb-2 flex flex-col items-center gap-1" style={{ left, width: boxW }}>
      {cue && sprite && (
        <div
          data-ckui="seek-sprite"
          className="rounded-md bg-black bg-no-repeat shadow-lg ring-1 ring-white/30"
          style={{
            width: w,
            height: cue.h * s,
            backgroundImage: `url("${cue.url}")`,
            backgroundPosition: `${-cue.x * s}px ${-cue.y * s}px`,
            backgroundSize: `${sprite.width * s}px ${sprite.height * s}px`,
          }}
        />
      )}
      <span className="rounded bg-black/75 px-1.5 py-0.5 text-xs font-medium text-white tabular-nums">{clock(time)}</span>
    </div>
  );
}

export function VolumeControl({ volume, muted, onVolume, onMute }: { volume: number; muted: boolean; onVolume: (v: number) => void; onMute: () => void }) {
  const { t } = useMessages();
  const silent = muted || volume === 0;
  return (
    <div className="group/volume flex items-center" data-ckui="volume">
      <ControlButton label={silent ? t("player.unmute") : t("player.mute")} shortcut="m" icon={silent ? VolumeOffIcon : volume < 0.5 ? VolumeLowIcon : VolumeHighIcon} onClick={onMute} />
      <div className="w-0 overflow-hidden opacity-0 transition-[width,opacity] group-focus-within/volume:w-20 group-focus-within/volume:opacity-100 group-hover/volume:w-20 group-hover/volume:opacity-100 motion-reduce:transition-none pointer-coarse:hidden">
        <SliderPrimitive.Root value={silent ? 0 : volume} min={0} max={1} step={0.05} onValueChange={(v) => onVolume(v)} className="px-2">
          <SliderPrimitive.Control className="relative flex h-6 w-full cursor-pointer touch-none items-center select-none">
            <SliderPrimitive.Track className="relative h-1 w-full overflow-hidden rounded-full bg-white/30">
              <SliderPrimitive.Indicator className="bg-white" />
            </SliderPrimitive.Track>
            <SliderPrimitive.Thumb
              aria-label={t("player.volume")}
              getAriaValueText={(_, v) => `${Math.round(v * 100)}%`}
              className="size-3 rounded-full bg-white outline-none focus-visible:ring-2 focus-visible:ring-white/80"
            />
          </SliderPrimitive.Control>
        </SliderPrimitive.Root>
      </div>
    </div>
  );
}

export interface MenuOption {
  value: string;
  label: string;
}

/** One settings row: its current value, and a submenu choosing it. */
export interface MenuSection {
  key: string;
  label: string;
  value: string;
  options: readonly MenuOption[];
  onChange: (value: string) => void;
}

export function SettingsMenu({
  icon,
  label,
  sections,
  actions,
  container,
  onOpenChange,
}: {
  icon: IconSvgElement;
  label: string;
  sections: MenuSection[];
  actions?: { label: string; onSelect: () => void }[];
  container?: HTMLElement;
  onOpenChange?: (open: boolean) => void;
}) {
  // Menus over video are dark, whatever the page's theme.
  const appearance = useContext(AppearanceContext);
  return (
    <AppearanceContext.Provider value={{ ...appearance, theme: "dark" }}>
      <DropdownMenu onOpenChange={onOpenChange}>
        <DropdownMenuTrigger aria-label={label} title={label} className={playerButtonClass} data-ckui="settings">
          <HugeiconsIcon icon={icon} strokeWidth={1.75} aria-hidden />
        </DropdownMenuTrigger>
        <DropdownMenuContent side="top" align="end" sideOffset={8} container={container} className="min-w-56" data-ckui="settings-menu">
          {sections.map((s) => (
            <DropdownMenuSub key={s.key}>
              <DropdownMenuSubTrigger data-ckui={`settings-${s.key}`}>
                <span>{s.label}</span>
                <span className="ml-auto max-w-32 truncate pl-4 text-muted-foreground">{s.options.find((o) => o.value === s.value)?.label}</span>
              </DropdownMenuSubTrigger>
              <DropdownMenuSubContent side="left" align="end" alignOffset={-4} container={container} className="min-w-40" data-ckui={`${s.key}-menu`}>
                <DropdownMenuRadioGroup value={s.value} onValueChange={(v) => v !== s.value && s.onChange(String(v))}>
                  {s.options.map((o) => (
                    <DropdownMenuRadioItem key={o.value} value={o.value} closeOnClick>
                      {o.label}
                    </DropdownMenuRadioItem>
                  ))}
                </DropdownMenuRadioGroup>
              </DropdownMenuSubContent>
            </DropdownMenuSub>
          ))}
          {actions?.length ? (
            <>
              {sections.length > 0 && <DropdownMenuSeparator />}
              {actions.map((a) => (
                <DropdownMenuItem key={a.label} onClick={a.onSelect}>
                  {a.label}
                </DropdownMenuItem>
              ))}
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
    </AppearanceContext.Provider>
  );
}

/** The showing subtitle cues, drawn above the controls; markup comes from the browser's cue parser. */
export function Captions({ cues, bottom }: { cues: readonly VTTCue[]; bottom: string }) {
  if (!cues.length) return null;
  return (
    <div
      data-ckui="captions"
      className="pointer-events-none absolute inset-x-0 z-[5] flex flex-col items-center gap-1 px-[5%] text-center transition-[bottom] duration-200 motion-reduce:transition-none"
      style={{ bottom }}
    >
      {cues.map((c, i) => (
        <Cue key={`${c.startTime}:${i}`} cue={c} />
      ))}
    </div>
  );
}

function Cue({ cue }: { cue: VTTCue }) {
  const ref = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const html = typeof cue.getCueAsHTML === "function" ? cue.getCueAsHTML() : null;
    el.replaceChildren(html ?? document.createTextNode(cue.text));
  }, [cue]);
  return <span ref={ref} className="rounded bg-black/75 px-2 py-0.5 text-[clamp(0.8rem,3.2cqw,2rem)] leading-snug whitespace-pre-line text-white" />;
}

export interface SeekFlashState {
  direction: -1 | 1;
  /** The burst's total, e.g. 30 after three taps. */
  seconds: number;
}

/** The "10 seconds" bubble for keyboard and double-tap seeks. */
export function SeekFlash({ flash }: { flash: SeekFlashState | null }) {
  const { t } = useMessages();
  if (!flash) return null;
  const back = flash.direction < 0;
  return (
    <div
      key={flash.direction}
      aria-hidden
      data-ckui="seek-flash"
      className={cn(
        "pointer-events-none absolute top-1/2 z-[6] -translate-y-1/2 duration-200 animate-in fade-in zoom-in-90 motion-reduce:animate-none",
        back ? "left-[8%]" : "right-[8%]",
      )}
    >
      <div className="flex size-20 flex-col items-center justify-center gap-0.5 rounded-full bg-black/50 text-white sm:size-24">
        <HugeiconsIcon icon={back ? ArrowLeft01Icon : ArrowRight01Icon} className="size-7" strokeWidth={2.5} />
        <span className="text-xs font-semibold tabular-nums">{t("player.seconds", { seconds: flash.seconds })}</span>
      </div>
    </div>
  );
}

export function Spinner({ className }: { className?: string }) {
  return <span aria-hidden className={cn("size-6 rounded-full border-2 border-white/30 border-t-white motion-safe:animate-spin", className)} />;
}


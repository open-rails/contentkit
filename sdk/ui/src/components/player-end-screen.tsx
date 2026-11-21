import { PlayIcon, Refresh01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useRef, useState, type MouseEvent } from "react";
import { Button } from "#ckui/ui/button";
import type { PublicRendition } from "../client/generated/wire.js";
import { useMessages } from "../i18n/context.js";
import { RenditionImg } from "./rendition-img.js";

/** A video the end screen offers: its link, title and cover, and an optional label ("Episode 3"). */
export interface PlayerEndItem {
  href: string;
  title: string;
  label?: string;
  cover?: readonly PublicRendition[] | null;
}

/** What follows the video once it ends, drawn by the player in place of Replay. */
export interface PlayerEndScreen {
  /** The video to continue with, e.g. a season's next episode. */
  next?: PlayerEndItem;
  /** Seconds before `next` opens by itself; without it the viewer chooses. */
  countdown?: number;
  /** Offered when there is no `next`; up to four are shown, two on a small player. */
  related?: readonly PlayerEndItem[];
  /** Opens an item: the host's navigation, so links stay in the app. */
  onSelect: (item: PlayerEndItem) => void;
}

const RELATED_SHOWN = 4;

export function EndScreen({ screen, onReplay }: { screen: PlayerEndScreen; onReplay: () => void }) {
  const { t, plural } = useMessages();
  const { next, countdown, related = [], onSelect } = screen;
  const [left, setLeft] = useState(countdown ?? 0);
  const [cancelled, setCancelled] = useState(false);
  const counting = !!next && !!countdown && !cancelled;
  const select = useRef(onSelect);
  select.current = onSelect;
  useEffect(() => {
    if (!counting) return;
    if (left <= 0) {
      select.current(next);
      return;
    }
    const tick = setTimeout(() => setLeft((s) => s - 1), 1000);
    return () => clearTimeout(tick);
  }, [counting, left, next]);

  const replay = (
    <Button size="sm" variant="ghost" className="text-white hover:bg-white/15 hover:text-white" onClick={onReplay}>
      <HugeiconsIcon icon={Refresh01Icon} data-icon="inline-start" />
      {t("player.replay")}
    </Button>
  );

  if (next)
    return (
      <div data-ckui="end-screen" className="absolute inset-0 z-[7] flex items-center justify-center bg-black/75 px-4 pb-16 @sm:pb-20">
        <div className="flex w-full max-w-md flex-col gap-3">
          <p className="text-sm text-white/70" aria-live="polite">
            {counting ? plural("player.upNextIn", left) : t("player.upNext")}
          </p>
          <Tile item={next} onSelect={onSelect} layout="row" />
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={() => onSelect(next)}>
              <HugeiconsIcon icon={PlayIcon} data-icon="inline-start" className="fill-current" />
              {t("player.playNow")}
            </Button>
            {counting ? (
              <Button size="sm" variant="ghost" className="text-white hover:bg-white/15 hover:text-white" onClick={() => setCancelled(true)}>
                {t("player.cancel")}
              </Button>
            ) : (
              replay
            )}
          </div>
        </div>
      </div>
    );

  const shown = related.slice(0, RELATED_SHOWN);
  return (
    <div data-ckui="end-screen" className="absolute inset-0 z-[7] flex flex-col items-center justify-center gap-3 bg-black/75 px-4 pb-16 @sm:pb-20">
      {shown.length > 0 && (
        <>
          <p className="text-sm text-white/70">{t("player.watchNext")}</p>
          <ul className="grid w-full max-w-md grid-cols-2 gap-3 @2xl:max-w-2xl @2xl:grid-cols-4">
            {shown.map((item, i) => (
              <li key={item.href} className={i >= 2 ? "hidden @2xl:block" : undefined}>
                <Tile item={item} onSelect={onSelect} layout="tile" />
              </li>
            ))}
          </ul>
        </>
      )}
      {replay}
    </div>
  );
}

// Covers need room: the next episode's from a medium player, the related tiles' from a large one.
function Tile({ item, onSelect, layout }: { item: PlayerEndItem; onSelect: (item: PlayerEndItem) => void; layout: "row" | "tile" }) {
  // A plain click stays in the app; modified clicks open the link as usual.
  const open = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    onSelect(item);
  };
  const row = layout === "row";
  return (
    <a
      href={item.href}
      onClick={open}
      className={cn("group flex gap-1.5 rounded-md text-white outline-none focus-visible:ring-2 focus-visible:ring-white/70", row ? "items-center gap-3" : "flex-col")}
    >
      <span className={cn("relative hidden aspect-video shrink-0 overflow-hidden rounded-md bg-white/10", row ? "w-36 @md:block" : "@xl:block")}>
        <RenditionImg outputs={item.cover} alt="" fit="cover" className="size-full transition-opacity group-hover:opacity-85" />
      </span>
      <span className={cn("line-clamp-2 leading-snug", row ? "text-sm font-medium" : "text-xs")}>
        {item.title}
        {item.label ? <span className="font-normal text-white/60"> · {item.label}</span> : null}
      </span>
    </a>
  );
}

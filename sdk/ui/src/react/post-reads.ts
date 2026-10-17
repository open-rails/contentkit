import { useCallback, useContext, useEffect, useRef, useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitContext } from "./context.js";
import { useReadingEngagement } from "./use-engagement.js";
import { useContentScope } from "./use-resource.js";

// A guest's posts read in this browser, and since when it has followed the list.
// Signed-in readers use their measured server history instead.
interface Reads {
  since: number;
  read: readonly string[];
}

const PREFIX = "ckui:post-reads:";
const MAX = 500;
const listeners = new Set<() => void>();
const snapshots = new Map<string, Reads>();

function storage(): Storage | null {
  try {
    return typeof window === "undefined" ? null : window.localStorage;
  } catch {
    return null;
  }
}

function load(scope: string): Reads {
  const cached = snapshots.get(scope);
  if (cached) return cached;
  let reads: Reads = { since: Date.now(), read: [] };
  try {
    const raw = storage()?.getItem(PREFIX + scope);
    const parsed = raw ? (JSON.parse(raw) as Partial<Reads>) : null;
    if (parsed && typeof parsed.since === "number" && Array.isArray(parsed.read)) reads = { since: parsed.since, read: parsed.read.filter((id) => typeof id === "string") };
  } catch {
    // unreadable storage: everything older than now counts as read
  }
  snapshots.set(scope, reads);
  return reads;
}

function save(scope: string, reads: Reads) {
  snapshots.set(scope, reads);
  try {
    storage()?.setItem(PREFIX + scope, JSON.stringify(reads));
  } catch {
    // private mode or full storage: keep this tab's copy
  }
  for (const l of listeners) l();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  const sync = (e: StorageEvent) => {
    if (!e.key?.startsWith(PREFIX)) return;
    snapshots.delete(e.key.slice(PREFIX.length));
    listener();
  };
  window.addEventListener("storage", sync);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", sync);
  };
}

export interface PostReads {
  /** Whether a post published at publishedAt is unread. Signed in, seen is the account's measured read. */
  unread: (id: string, publishedAt?: string, seen?: boolean) => boolean;
}

/**
 * Unread posts. Signed in: not measured as read and published within the last
 * windowDays. A guest: published since this browser started following the
 * list and not read here.
 */
export function usePostReads(windowDays = 30): PostReads {
  const { scope } = useContentScope();
  const member = useContext(ContentKitContext)?.viewer != null;
  const reads = useSyncExternalStore(subscribe, () => load(scope), () => load(scope));
  useEffect(() => {
    if (member) return;
    try {
      if (!storage()?.getItem(PREFIX + scope)) save(scope, load(scope));
    } catch {
      // storage unavailable
    }
  }, [scope, member]);
  const unread = useCallback(
    (id: string, publishedAt?: string, seen?: boolean) => {
      if (!publishedAt) return false;
      if (member) return !seen && Date.parse(publishedAt) > Date.now() - windowDays * 86_400_000;
      return Date.parse(publishedAt) > reads.since && !reads.read.includes(id);
    },
    [reads, member, windowDays],
  );
  return { unread };
}

export interface PostReadingOptions {
  client?: ContentKitClient;
  kind?: string;
  /** Visible, focused attention before a guest's browser counts the post read; default 3 seconds. */
  guestReadAfter?: number;
}

/**
 * Attach to a text post's body. Its visible, focused attention is measured
 * (never a raw open), so the account's history marks it read; a guest's
 * browser marks it read after guestReadAfter of attention.
 */
export function usePostReading(contentId: string | null | undefined, versionId?: string, options: PostReadingOptions = {}): (element: HTMLElement | null) => void {
  const { scope } = useContentScope();
  const guest = useContext(ContentKitContext)?.viewer === null;
  const engagement = useReadingEngagement(contentId, versionId, { client: options.client, kind: options.kind });
  const body = useRef<HTMLElement | null>(null);
  const after = options.guestReadAfter ?? 3000;
  useEffect(() => {
    if (!guest || !contentId || load(scope).read.includes(contentId)) return;
    let attended = 0;
    const tick = 500;
    const timer = setInterval(() => {
      const box = body.current?.getBoundingClientRect();
      const shown = !!box && box.height > 0 && box.bottom > 0 && box.top < window.innerHeight;
      if (!shown || document.visibilityState === "hidden" || !document.hasFocus()) return;
      if ((attended += tick) < after) return;
      clearInterval(timer);
      const reads = load(scope);
      if (!reads.read.includes(contentId)) save(scope, { since: reads.since, read: [contentId, ...reads.read].slice(0, MAX) });
    }, tick);
    return () => clearInterval(timer);
  }, [guest, contentId, scope, after]);
  const { text } = engagement;
  return useCallback(
    (element: HTMLElement | null) => {
      body.current = element;
      text(element);
    },
    [text],
  );
}

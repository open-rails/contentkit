import { useCallback, useEffect, useSyncExternalStore } from "react";
import { useContentScope } from "./use-resource.js";

// Posts this browser opened, and since when it has followed the list. A post is
// unread when published after that and opened neither here nor by the account.
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
  /** Whether a post published at publishedAt is unread here, unless the account already saw it. */
  unread: (id: string, publishedAt?: string, seen?: boolean) => boolean;
}

/** This browser's read posts; the first visit starts following from now. */
export function usePostReads(): PostReads {
  const { scope } = useContentScope();
  const reads = useSyncExternalStore(subscribe, () => load(scope), () => load(scope));
  useEffect(() => {
    try {
      if (!storage()?.getItem(PREFIX + scope)) save(scope, load(scope));
    } catch {
      // storage unavailable
    }
  }, [scope]);
  const unread = useCallback(
    (id: string, publishedAt?: string, seen?: boolean) => !seen && !!publishedAt && Date.parse(publishedAt) > reads.since && !reads.read.includes(id),
    [reads],
  );
  return { unread };
}

/** Marks a post read in this browser once its page shows it. */
export function useMarkPostRead(id: string | null | undefined): void {
  const { scope } = useContentScope();
  useEffect(() => {
    if (!id) return;
    const reads = load(scope);
    if (reads.read.includes(id)) return;
    save(scope, { since: reads.since, read: [id, ...reads.read].slice(0, MAX) });
  }, [id, scope]);
}

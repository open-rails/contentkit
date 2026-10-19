import type { Card, EngagementSnapshot, ViewerState } from "./generated/wire.js";

export type GuestReadingSnapshot = EngagementSnapshot & { kind?: string };
interface Checkpoint extends Omit<GuestReadingSnapshot, "activity_token"> { saved_at: number }
export interface GuestReadingItem { id: string; kind?: string; at: number; checkpoints: readonly Checkpoint[] }
const NONE: readonly GuestReadingItem[] = Object.freeze([]);
const MAX_CONTENTS = 100, MAX_SEGMENTS = 2000;
const integer = (n: unknown, max = Number.MAX_SAFE_INTEGER): n is number => typeof n === "number" && Number.isSafeInteger(n) && n >= 0 && n <= max;

/** A separate key per cumulative segment avoids cross-tab read/modify/write loss. No titles, media URLs or account data are stored. */
export class GuestReadingHistory {
  private rows: readonly GuestReadingItem[] = NONE;
  private encoded = "[]";
  private memory = new Map<string, Checkpoint>();
  private readonly listeners = new Set<() => void>();
  constructor(private readonly prefix: string, private readonly browser?: Window) { this.sync(); }
  readonly snapshot = (): readonly GuestReadingItem[] => this.rows;
  readonly subscribe = (listener: () => void): (() => void) => {
    this.sync();
    if (!this.listeners.size) this.browser?.addEventListener("storage", this.storage);
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); if (!this.listeners.size) this.browser?.removeEventListener("storage", this.storage); };
  };
  record(snapshot: GuestReadingSnapshot): void {
    if (!snapshot.content_id || !snapshot.visit_id || !snapshot.segment_id || !integer(snapshot.revision) || !integer(snapshot.active_ms, 600_000) || !snapshot.active_ms) return;
    if ((snapshot.pages?.length ?? 0) > 128 || (snapshot.watched?.length ?? 0) > 256) return;
    if (snapshot.pages?.some(p => !integer(p.index) || !integer(p.active_ms, 600_000) || p.ranges.length > 16 || p.ranges.some(r => !integer(r.from, 10_000) || !integer(r.to, 10_000) || r.from >= r.to))) return;
    if (snapshot.watched?.some(r => !integer(r.from_ms, 86_400_000) || !integer(r.to_ms, 86_400_000) || r.from_ms >= r.to_ms)) return;
    const { activity_token: _token, ...local } = snapshot;
    const key = this.prefix + encodeURIComponent(JSON.stringify([snapshot.content_id, snapshot.version_id ?? "", snapshot.visit_id, snapshot.segment_id]));
    const existing = this.read().get(key);
    if (existing && existing.revision >= snapshot.revision) return;
    const value = { ...local, saved_at: Date.now() };
    if (JSON.stringify(value).length > 64 * 1024) return;
    this.write(key, value);
    this.prune(); this.sync();
  }
  clear(filter: { content_id?: string; kind?: string } = {}): void {
    for (const [key, row] of this.read()) if ((!filter.content_id || filter.content_id === row.content_id) && (!filter.kind || filter.kind === row.kind)) this.remove(key);
    this.sync();
  }
  private readonly storage = (event: StorageEvent): void => { if (event.key === null || event.key.startsWith(this.prefix)) this.sync(); };
  private read(): Map<string, Checkpoint> {
    if (!this.browser) return new Map(this.memory);
    const out = new Map(this.memory);
    try {
      for (let i = 0; i < this.browser.localStorage.length; i++) {
        const key = this.browser.localStorage.key(i);
        if (!key?.startsWith(this.prefix)) continue;
        try {
          const raw = this.browser.localStorage.getItem(key);
          if (!raw || raw.length > 64 * 1024) continue;
          const row = JSON.parse(raw) as Checkpoint | null;
          if (row && (!row.pages || Array.isArray(row.pages) && row.pages.length <= 128 && row.pages.every(p => p && integer(p.index) && integer(p.active_ms, 600_000) && Array.isArray(p.ranges) && p.ranges.length <= 16 && p.ranges.every(r => r && integer(r.from, 10_000) && integer(r.to, 10_000) && r.from < r.to))) && (!row.watched || Array.isArray(row.watched) && row.watched.length <= 256 && row.watched.every(r => r && integer(r.from_ms, 86_400_000) && integer(r.to_ms, 86_400_000) && r.from_ms < r.to_ms)) && typeof row.content_id === "string" && typeof row.visit_id === "string" && typeof row.segment_id === "string" && integer(row.revision) && integer(row.saved_at) && integer(row.active_ms, 600_000) && row.active_ms > 0) out.set(key, row);
        } catch { /* Ignore a malformed browser-only checkpoint. */ }
      }
    } catch { /* Storage can be unavailable; this client keeps bounded memory. */ }
    return out;
  }
  private write(key: string, row: Checkpoint): void {
    try { if (this.browser) { this.browser.localStorage.setItem(key, JSON.stringify(row)); this.memory.delete(key); return; } } catch { /* Quota/privacy mode. */ }
    this.memory.set(key, row);
  }
  private remove(key: string): void { this.memory.delete(key); try { this.browser?.localStorage.removeItem(key); } catch { /* Browser policy. */ } }
  private prune(): void {
    const groups = new Map<string, { keys: string[]; at: number }>();
    for (const [key, row] of this.read()) {
      const value = groups.get(row.content_id) ?? { keys: [], at: row.saved_at };
      value.keys.push(key); value.at = Math.max(value.at, row.saved_at); groups.set(row.content_id, value);
    }
    let contents = 0, segments = 0;
    for (const value of [...groups.values()].sort((a, b) => b.at - a.at)) {
      // Evict a whole older content item; never silently turn a retained history item into partial coverage.
      if (contents >= MAX_CONTENTS || segments + value.keys.length > MAX_SEGMENTS) {
        for (const key of value.keys) this.remove(key);
      } else { contents++; segments += value.keys.length; }
    }
  }

  private sync(): void {
    const byContent = new Map<string, { id: string; kind?: string; at: number; checkpoints: Checkpoint[] }>();
    for (const row of this.read().values()) {
      const item = byContent.get(row.content_id) ?? { id: row.content_id, kind: row.kind, at: row.saved_at, checkpoints: [] };
      item.at = Math.max(item.at, row.saved_at); item.checkpoints.push(row); byContent.set(item.id, item);
    }
    for (const row of byContent.values()) row.checkpoints.sort((a, b) => a.visit_id.localeCompare(b.visit_id) || a.segment_id.localeCompare(b.segment_id));
    const rows = [...byContent.values()].sort((a, b) => b.at - a.at || a.id.localeCompare(b.id));
    const encoded = JSON.stringify(rows);
    if (encoded === this.encoded) return;
    this.encoded = encoded; this.rows = Object.freeze(rows.map(row => Object.freeze(row)));
    for (const listener of this.listeners) { try { listener(); } catch { /* Observers do not own persistence. */ } }
  }
}

/** Hydrate through ordinary public discovery before applying local progress: cached IDs never bypass current domain/access restrictions. */
export function guestStanding(card: Card, item: GuestReadingItem): ViewerState {
  const rows = item.checkpoints.filter(row => !row.version_id || row.version_id === card.version.id);
  const pages = new Map<number, { active: number; ranges: { from: number; to: number }[] }>();
  const watched: { from: number; to: number }[] = [];
  for (const row of rows) {
    for (const page of row.pages ?? []) {
      const value = pages.get(page.index) ?? { active: 0, ranges: [] };
      value.active += page.active_ms; value.ranges.push(...page.ranges); pages.set(page.index, value);
    }
    watched.push(...(row.watched ?? []).map(r => ({ from: r.from_ms, to: r.to_ms })));
  }
  const unionLength = (ranges: { from: number; to: number }[]) => {
    let end = 0, n = 0;
    for (const r of ranges.sort((a, b) => a.from - b.from)) { n += Math.max(0, r.to - Math.max(end, r.from)); end = Math.max(end, r.to); }
    return n;
  };
  const pageCount = card.version.pages ?? 0;
  const total = pageCount ? pageCount * 100 : Math.ceil((card.version.duration_ms ?? 0) / 1000);
  // Hundredths of a page touched, as the server stores them: each range widens to whole hundredths.
  const hundredths = (ranges: { from: number; to: number }[]) => {
    const touched = new Set<number>();
    for (const r of ranges) for (let n = Math.floor(r.from / 100); n < Math.ceil(r.to / 100); n++) touched.add(n);
    return touched.size;
  };
  const covered = pageCount ? [...pages].reduce((n, [index, page]) => n + (index < pageCount && page.active >= 1000 ? hundredths(page.ranges) : 0), 0) : unionLength(watched) / 1000;
  // Readers report whole pages, as accounts do: hundredths rounded down, at least 1 once any are read.
  const progress = pageCount ? (covered > 0 ? Math.max(1, Math.floor(covered / 100)) : 0) : Math.floor(covered);
  const newest = rows.toSorted((a, b) => b.saved_at - a.saved_at)[0];
  return { favorited: card.viewer?.favorited ?? false, seen: rows.some(row => row.active_ms > 0), completed: total > 0 && covered >= total * 0.9,
    progress: Math.min(pageCount || total, progress), progress_max: pageCount || total, resume: newest?.resume };
}

const stores = new WeakMap<Window, Map<string, GuestReadingHistory>>();
export function guestReadingHistory(mount: string): GuestReadingHistory {
  const browser = typeof window === "undefined" ? undefined : window;
  const key = `contentkit.reading.v1:${encodeURIComponent(mount)}:`;
  if (!browser) return new GuestReadingHistory(key);
  let scoped = stores.get(browser); if (!scoped) { scoped = new Map(); stores.set(browser, scoped); }
  let store = scoped.get(key); if (!store) { store = new GuestReadingHistory(key, browser); scoped.set(key, store); }
  return store;
}

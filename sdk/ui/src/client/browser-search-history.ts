import type { RecentSearch } from "./generated/wire.js";

const MAX_SEARCHES = 100;
const normalized = (query: string) => query.trim().replace(/\s+/g, " ");
const identity = (query: string) => normalized(query).toLowerCase();

/** The number of recent queries displayed, bounded like the server endpoint. */
export const recentSearchLimit = (limit = 10): number => Number.isFinite(limit) ? Math.max(1, Math.min(MAX_SEARCHES, Math.floor(limit))) : 10;

function decode(raw: string | null): RecentSearch[] {
  try {
    const data: unknown = JSON.parse(raw ?? "[]");
    if (!Array.isArray(data)) return [];
    const seen = new Set<string>();
    const rows: RecentSearch[] = [];
    for (const entry of data) {
      if (!entry || typeof entry.query !== "string" || typeof entry.results !== "number" || !Number.isFinite(entry.results) || entry.results < 0 || typeof entry.searched_at !== "string" || !Number.isFinite(Date.parse(entry.searched_at))) continue;
      const query = normalized(entry.query), key = identity(query);
      if (!key || seen.has(key)) continue;
      seen.add(key);
      rows.push({ query, results: Math.floor(entry.results), searched_at: entry.searched_at });
      if (rows.length === MAX_SEARCHES) break;
    }
    return rows;
  } catch { return []; }
}

/** Browser-owned anonymous search history. It never reads or copies account history. */
export class BrowserSearchHistory {
  private rows: readonly RecentSearch[] = [];
  private encoded = "[]";
  private storageFailed = false;
  private readonly listeners = new Set<() => void>();

  constructor(private readonly key: string, private readonly browser?: Window) { this.sync(); }

  readonly snapshot = (): readonly RecentSearch[] => this.rows;

  list(limit?: number): RecentSearch[] {
    this.sync();
    return this.rows.slice(0, recentSearchLimit(limit)).map((row) => ({ ...row }));
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.sync();
    if (!this.listeners.size) this.browser?.addEventListener("storage", this.onStorage);
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
      if (!this.listeners.size) this.browser?.removeEventListener("storage", this.onStorage);
    };
  };

  record(query: string, results: number): void {
    query = normalized(query);
    if (!query) return;
    this.sync();
    this.save([{ query, results: Number.isFinite(results) ? Math.max(0, Math.floor(results)) : 0, searched_at: new Date().toISOString() }, ...this.rows.filter((row) => identity(row.query) !== identity(query))].slice(0, MAX_SEARCHES));
  }

  clear(query?: string): void {
    this.sync();
    this.save(query ? this.rows.filter((row) => identity(row.query) !== identity(query)) : []);
  }

  private readonly onStorage = (event: StorageEvent): void => {
    if (event.key === null || event.key === this.key) this.sync();
  };

  private sync(): void {
    if (!this.browser || this.storageFailed) return;
    try { this.replace(decode(this.browser.localStorage.getItem(this.key))); }
    catch { this.storageFailed = true; }
  }

  private save(rows: readonly RecentSearch[]): void {
    if (this.browser && !this.storageFailed) {
      try {
        if (rows.length) this.browser.localStorage.setItem(this.key, JSON.stringify(rows));
        else this.browser.localStorage.removeItem(this.key);
      } catch { this.storageFailed = true; }
    }
    this.replace(rows);
  }

  private replace(rows: readonly RecentSearch[]): void {
    const encoded = JSON.stringify(rows);
    if (encoded === this.encoded) return;
    this.encoded = encoded;
    this.rows = Object.freeze(rows.map((row) => Object.freeze({ ...row })));
    for (const listener of this.listeners) {
      try { listener(); } catch { /* A subscriber must not fail the search that completed. */ }
    }
  }
}

const stores = new WeakMap<Window, Map<string, BrowserSearchHistory>>();

/** One same-tab store per discovery URL; server rendering never shares a store across requests. */
export function browserSearchHistory(mount: string): BrowserSearchHistory {
  const browser = typeof window === "undefined" ? undefined : window;
  let scope = mount.replace(/\/+$/, "");
  if (browser) {
    try { scope = new URL(scope, browser.location.href).href; } catch { /* A relative SSR mount remains a distinct local key. */ }
  }
  const key = `contentkit:recent-searches:v1:${scope}`;
  if (!browser) return new BrowserSearchHistory(key);
  let scoped = stores.get(browser);
  if (!scoped) stores.set(browser, (scoped = new Map()));
  let store = scoped.get(key);
  if (!store) scoped.set(key, (store = new BrowserSearchHistory(key, browser)));
  return store;
}

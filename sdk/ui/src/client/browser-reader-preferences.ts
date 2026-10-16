import type { FilterSetting } from "./generated/wire.js";

/** Explicit visibility or a reset that follows the current domain default. */
export type ReaderFilterChoice = { entity_id: string; visible: boolean; default?: never } | { entity_id: string; default: true; visible?: never };
export type ReaderFilterKinds = Record<string, readonly ReaderFilterChoice[]>;

export interface GuestReaderPreferences {
  language?: string;
  kinds: ReaderFilterKinds;
  /** Choices saved before content kinds were introduced. Explicit kind choices win. */
  legacy_filters: readonly FilterSetting[];
}

export interface PendingReaderPreferences extends GuestReaderPreferences {
  account: string;
  languageDone: boolean;
  filtersDone: boolean;
  languageObserved?: boolean;
  initialLanguage?: string | null;
}

type Document = GuestReaderPreferences & { registrations: readonly PendingReaderPreferences[] };
const EMPTY: Document = Object.freeze({ kinds: Object.freeze({}), legacy_filters: Object.freeze([]), registrations: Object.freeze([]) });

function language(value: unknown): string | undefined {
  if (typeof value !== "string" || value.length > 64) return;
  try { return Intl.getCanonicalLocales(value)[0]; } catch { return; }
}

function filters(value: unknown): FilterSetting[] {
  if (!Array.isArray(value)) return [];
  const entries = new Map<string, boolean>();
  for (const row of value) {
    if (row && typeof (row.entity_id ?? row.group_id) === "string" && (row.entity_id ?? row.group_id).length > 0 && (row.entity_id ?? row.group_id).length <= 128 && typeof row.visible === "boolean") entries.set(row.entity_id ?? row.group_id, row.visible);
  }
  return [...entries].sort(([a], [b]) => a.localeCompare(b)).map(([entity_id, visible]) => ({ entity_id, visible }));
}

function scopedFilters(value: unknown): ReaderFilterChoice[] {
  if (!Array.isArray(value)) return [];
  const entries = new Map<string, ReaderFilterChoice>();
  for (const row of value) {
    const id = row?.entity_id;
    if (typeof id !== "string" || !id || id.length > 128) continue;
    if (row.default === true && row.visible === undefined) entries.set(id, { entity_id: id, default: true });
    else if (row.default === undefined && typeof row.visible === "boolean") entries.set(id, { entity_id: id, visible: row.visible });
  }
  return [...entries].sort(([a], [b]) => a.localeCompare(b)).map(([, row]) => row);
}

function kinds(value: unknown): ReaderFilterKinds {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return Object.fromEntries(Object.entries(value)
    .filter(([kind]) => /^[a-z][a-z0-9_]*$/.test(kind))
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([kind, value]) => [kind, scopedFilters(value)]));
}

function choices(value: Record<string, unknown>) {
  return { kinds: kinds(value.kinds), legacy_filters: filters(value.legacy_filters ?? value.filters) };
}

function hasChoices(value: GuestReaderPreferences): boolean {
  return value.legacy_filters.length > 0 || Object.values(value.kinds).some((rows) => rows.length > 0);
}

function freezeChoices(value: GuestReaderPreferences): GuestReaderPreferences {
  return { language: value.language,
    kinds: Object.freeze(Object.fromEntries(Object.entries(value.kinds).map(([kind, rows]) => [kind, Object.freeze(rows.map((f) => Object.freeze({ ...f })))]))),
    legacy_filters: Object.freeze(value.legacy_filters.map((f) => Object.freeze({ ...f }))) };
}

function decode(raw: string | null): Document {
  try {
    const data: unknown = JSON.parse(raw ?? "{}");
    if (!data || typeof data !== "object") return EMPTY;
    const value = data as Record<string, unknown>;
    const registrations: PendingReaderPreferences[] = [];
    if (Array.isArray(value.registrations)) {
      for (const entry of value.registrations.slice(0, 20)) {
        if (!entry || typeof entry.account !== "string" || !entry.account || entry.account.length > 128 || registrations.some((r) => r.account === entry.account)) continue;
        registrations.push({ account: entry.account, language: language(entry.language), ...choices(entry), languageDone: entry.languageDone === true, filtersDone: entry.filtersDone === true,
          languageObserved: entry.languageObserved === true, initialLanguage: entry.initialLanguage === null ? null : language(entry.initialLanguage) });
      }
    }
    return { language: language(value.language), ...choices(value), registrations };
  } catch { return EMPTY; }
}

/** Anonymous preferences and registration retries, isolated to one app URL. */
export class BrowserReaderPreferences {
  private value: Document = EMPTY;
  private publicValue: GuestReaderPreferences = EMPTY;
  private encoded = JSON.stringify(EMPTY);
  private unavailable = false;
  private readonly listeners = new Set<() => void>();

  constructor(private readonly key: string, private readonly browser?: Window) { this.sync(); }

  readonly snapshot = (): GuestReaderPreferences => this.publicValue;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.sync();
    if (!this.listeners.size) this.browser?.addEventListener("storage", this.onStorage);
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
      if (!this.listeners.size) this.browser?.removeEventListener("storage", this.onStorage);
    };
  };

  setLanguage(value: string): void {
    const next = language(value);
    if (!next) throw new Error("contentkit: invalid preferred language");
    this.sync();
    this.save({ ...this.value, language: next });
  }

  setFilters(value: ReaderFilterKinds): void {
    this.sync();
    this.save({ ...this.value, kinds: kinds(value) });
  }

  pending(account: string): PendingReaderPreferences | undefined {
    this.sync();
    return this.value.registrations.find((r) => r.account === account);
  }

  /** Capture only on a confirmed registration, never on a login transition. */
  register(account: string): void {
    this.sync();
    if (!account || account.length > 128) throw new Error("contentkit: registration account is required");
    if (this.pending(account)) return;
    if (!this.value.language && !hasChoices(this.value)) return;
    if (this.value.registrations.length >= 20) throw new Error("contentkit: too many pending preference registrations");
    this.save({ ...this.value, registrations: [...this.value.registrations, {
      account, language: this.value.language, kinds: this.value.kinds, legacy_filters: this.value.legacy_filters,
      languageDone: !this.value.language, filtersDone: !hasChoices(this.value),
    }] });
  }

  complete(account: string, step: "language" | "filters"): void {
    this.sync();
    const registrations = this.value.registrations.map((r) => r.account === account ? { ...r, [step === "language" ? "languageDone" : "filtersDone"]: true } : r)
      .filter((r) => !r.languageDone || !r.filtersDone);
    this.save({ ...this.value, registrations });
  }

  observeLanguage(account: string, initialLanguage: string | null): void {
    this.sync();
    this.save({ ...this.value, registrations: this.value.registrations.map((r) => r.account === account && !r.languageObserved ? { ...r, languageObserved: true, initialLanguage } : r) });
  }

  private readonly onStorage = (event: StorageEvent): void => {
    if (event.key === null || event.key === this.key) this.sync();
  };

  private sync(): void {
    if (!this.browser || this.unavailable) return;
    try { this.replace(decode(this.browser.localStorage.getItem(this.key))); }
    catch { this.unavailable = true; }
  }

  private save(next: Document): void {
    if (this.browser && !this.unavailable) {
      try { this.browser.localStorage.setItem(this.key, JSON.stringify(next)); }
      catch { this.unavailable = true; }
    }
    this.replace(next);
  }

  private replace(next: Document): void {
    const encoded = JSON.stringify({ language: next.language, kinds: next.kinds, legacy_filters: next.legacy_filters, registrations: next.registrations });
    if (encoded === this.encoded) return;
    this.encoded = encoded;
    this.value = Object.freeze({ ...freezeChoices(next), registrations: Object.freeze(next.registrations.map((r) => Object.freeze({ ...r, ...freezeChoices(r) }))) });
    const publicValue = { language: this.value.language, kinds: this.value.kinds, legacy_filters: this.value.legacy_filters };
    if (JSON.stringify(publicValue) !== JSON.stringify(this.publicValue)) this.publicValue = Object.freeze(publicValue);
    for (const listener of this.listeners) { try { listener(); } catch { /* Observers do not own persistence. */ } }
  }
}

const stores = new WeakMap<Window, Map<string, BrowserReaderPreferences>>();

/** Same-app clients share a store; server-rendered requests never do. */
export function browserReaderPreferences(mount: string): BrowserReaderPreferences {
  const browser = typeof window === "undefined" ? undefined : window;
  let scope = mount.replace(/\/+$/, "");
  if (browser) { try { scope = new URL(scope, browser.location.href).href; } catch { /* Keep the explicit app key. */ } }
  const key = `contentkit:reader-preferences:v1:${scope}`;
  if (!browser) return new BrowserReaderPreferences(key);
  let scoped = stores.get(browser);
  if (!scoped) stores.set(browser, (scoped = new Map()));
  let store = scoped.get(key);
  if (!store) scoped.set(key, (store = new BrowserReaderPreferences(key, browser)));
  return store;
}

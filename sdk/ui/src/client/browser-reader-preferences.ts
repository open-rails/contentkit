/** Explicit visibility or a reset that follows the current domain default. */
export type ReaderFilterChoice =
  | { entity_id: string; visible: boolean; default?: never }
  | { entity_id: string; default: true; visible?: never };
export type ReaderFilters = readonly ReaderFilterChoice[];

export interface GuestReaderPreferences {
  language?: string;
  filters: ReaderFilters;
}
export interface PendingReaderPreferences extends GuestReaderPreferences {
  account: string;
  languageDone: boolean;
  filtersDone: boolean;
  languageObserved?: boolean;
  initialLanguage?: string | null;
}

type Document = GuestReaderPreferences & { registrations: readonly PendingReaderPreferences[] };
const EMPTY: Document = Object.freeze({ filters: Object.freeze([]), registrations: Object.freeze([]) });

function language(value: unknown): string | undefined {
  if (typeof value !== "string" || value.length > 64) return;
  try {
    return Intl.getCanonicalLocales(value)[0];
  } catch {
    return;
  }
}
function filters(value: unknown): ReaderFilterChoice[] {
  if (!Array.isArray(value)) return [];
  const entries = new Map<string, ReaderFilterChoice>();
  for (const row of value) {
    const id = row?.entity_id;
    if (typeof id !== "string" || !id || id.length > 128) continue;
    if (row.default === true && row.visible === undefined) entries.set(id, { entity_id: id, default: true });
    else if (row.default === undefined && typeof row.visible === "boolean")
      entries.set(id, { entity_id: id, visible: row.visible });
  }
  return [...entries].sort(([a], [b]) => a.localeCompare(b)).map(([, row]) => row);
}

// Mirrors readerprefs.Stored: scoped hide wins show; a scoped reset suppresses
// the older unscoped fallback. New v2 documents never consult legacy values.
function legacyFilters(value: Record<string, unknown>): ReaderFilterChoice[] {
  const fallback = new Map<string, ReaderFilterChoice>();
  const old = value.legacy_filters ?? value.filters;
  if (Array.isArray(old))
    for (const row of old) {
      const id = row?.entity_id ?? row?.group_id;
      if (typeof id === "string" && id && id.length <= 128 && typeof row.visible === "boolean")
        fallback.set(id, { entity_id: id, visible: row.visible });
    }
  const scoped = new Map<string, { rank: number; choice: ReaderFilterChoice }>();
  if (value.kinds && typeof value.kinds === "object" && !Array.isArray(value.kinds))
    for (const rows of Object.values(value.kinds))
      for (const choice of filters(rows)) {
        const rank = choice.default ? 1 : choice.visible ? 2 : 3;
        if ((scoped.get(choice.entity_id)?.rank ?? 0) < rank) scoped.set(choice.entity_id, { rank, choice });
      }
  for (const [id, { choice }] of scoped) fallback.set(id, choice);
  return [...fallback].sort(([a], [b]) => a.localeCompare(b)).map(([, choice]) => choice);
}
function freezeChoices(value: GuestReaderPreferences): GuestReaderPreferences {
  return { language: value.language, filters: Object.freeze(value.filters.map((row) => Object.freeze({ ...row }))) };
}
function decode(raw: string | null, legacy = false): Document {
  try {
    const parsed: unknown = JSON.parse(raw ?? "{}");
    if (!parsed || typeof parsed !== "object") return EMPTY;
    const value = parsed as Record<string, unknown>;
    const choices = (entry: Record<string, unknown>) => ({
      language: language(entry.language),
      filters: legacy ? legacyFilters(entry) : filters(entry.filters),
    });
    const registrations: PendingReaderPreferences[] = [];
    if (Array.isArray(value.registrations))
      for (const entry of value.registrations.slice(0, 20)) {
        if (
          !entry ||
          typeof entry.account !== "string" ||
          !entry.account ||
          entry.account.length > 128 ||
          registrations.some((r) => r.account === entry.account)
        )
          continue;
        registrations.push({
          account: entry.account,
          ...choices(entry),
          languageDone: entry.languageDone === true,
          filtersDone: entry.filtersDone === true,
          languageObserved: entry.languageObserved === true,
          initialLanguage: entry.initialLanguage === null ? null : language(entry.initialLanguage),
        });
      }
    return { ...choices(value), registrations };
  } catch {
    return EMPTY;
  }
}

/** Anonymous choices and registration retries, isolated to one catalog URL.
 * v1 is read once when v2 is absent, and never modified, so old tabs cannot
 * clobber a new shared choice and a rollback still has the original data. */
export class BrowserReaderPreferences {
  private value: Document = EMPTY;
  private publicValue: GuestReaderPreferences = Object.freeze({ filters: Object.freeze([]) });
  private encoded = JSON.stringify(EMPTY);
  private unavailable = false;
  private readonly listeners = new Set<() => void>();
  constructor(
    private readonly key: string,
    private readonly browser?: Window,
    private readonly legacyKey?: string,
  ) {
    this.sync();
  }
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
  setFilters(value: ReaderFilters, validate?: (next: GuestReaderPreferences) => void): void {
    this.sync();
    const merged = new Map(this.value.filters.map((row) => [row.entity_id, row]));
    for (const row of filters(value)) merged.set(row.entity_id, row);
    const next = {
      ...this.value,
      filters: [...merged.values()].sort((a, b) => a.entity_id.localeCompare(b.entity_id)),
    };
    validate?.(next);
    this.save(next);
  }
  pending(account: string): PendingReaderPreferences | undefined {
    this.sync();
    return this.value.registrations.find((r) => r.account === account);
  }
  register(account: string): void {
    this.sync();
    if (!account || account.length > 128) throw new Error("contentkit: registration account is required");
    if (this.pending(account) || (!this.value.language && !this.value.filters.length)) return;
    if (this.value.registrations.length >= 20) throw new Error("contentkit: too many pending preference registrations");
    this.save({
      ...this.value,
      registrations: [
        ...this.value.registrations,
        {
          account,
          language: this.value.language,
          filters: this.value.filters,
          languageDone: !this.value.language,
          filtersDone: !this.value.filters.length,
        },
      ],
    });
  }
  complete(account: string, step: "language" | "filters"): void {
    this.sync();
    const registrations = this.value.registrations
      .map((r) => (r.account === account ? { ...r, [step === "language" ? "languageDone" : "filtersDone"]: true } : r))
      .filter((r) => !r.languageDone || !r.filtersDone);
    this.save({ ...this.value, registrations });
  }
  observeLanguage(account: string, initialLanguage: string | null): void {
    this.sync();
    this.save({
      ...this.value,
      registrations: this.value.registrations.map((r) =>
        r.account === account && !r.languageObserved ? { ...r, languageObserved: true, initialLanguage } : r,
      ),
    });
  }
  private readonly onStorage = (event: StorageEvent): void => {
    if (event.key === null || event.key === this.key) this.sync();
  };

  private sync(): void {
    if (!this.browser || this.unavailable) return;
    try {
      const raw = this.browser.localStorage.getItem(this.key);
      if (raw === null && this.legacyKey) {
        const old = this.browser.localStorage.getItem(this.legacyKey);
        if (old !== null) {
          this.save(decode(old, true));
          return;
        }
      }
      this.replace(decode(raw));
    } catch {
      this.unavailable = true;
    }
  }

  private save(next: Document): void {
    if (this.browser && !this.unavailable) {
      try {
        this.browser.localStorage.setItem(this.key, JSON.stringify(next));
      } catch {
        this.unavailable = true;
      }
    }
    this.replace(next);
  }

  private replace(next: Document): void {
    const encoded = JSON.stringify({
      language: next.language,
      filters: next.filters,
      registrations: next.registrations,
    });
    if (encoded === this.encoded) return;
    this.encoded = encoded;
    this.value = Object.freeze({
      ...freezeChoices(next),
      registrations: Object.freeze(next.registrations.map((r) => Object.freeze({ ...r, ...freezeChoices(r) }))),
    });
    const publicValue = { language: this.value.language, filters: this.value.filters };
    if (JSON.stringify(publicValue) !== JSON.stringify(this.publicValue)) this.publicValue = Object.freeze(publicValue);
    for (const listener of this.listeners) {
      try {
        listener();
      } catch {
        /* Observers do not own persistence. */
      }
    }
  }
}

const stores = new WeakMap<Window, Map<string, BrowserReaderPreferences>>();

/** Same-app clients share a store; server-rendered requests never do. */
export function browserReaderPreferences(mount: string): BrowserReaderPreferences {
  const browser = typeof window === "undefined" ? undefined : window;
  let scope = mount.replace(/\/+$/, "");
  if (browser) {
    try {
      scope = new URL(scope, browser.location.href).href;
    } catch {
      /* Keep the explicit app key. */
    }
  }
  const key = `contentkit:reader-preferences:v2:${scope}`;
  const legacyKey = `contentkit:reader-preferences:v1:${scope}`;
  if (!browser) return new BrowserReaderPreferences(key);
  let scoped = stores.get(browser);
  if (!scoped) stores.set(browser, (scoped = new Map()));
  let store = scoped.get(key);
  if (!store) scoped.set(key, (store = new BrowserReaderPreferences(key, browser, legacyKey)));
  return store;
}

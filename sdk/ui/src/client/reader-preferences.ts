import type { ReaderFilters } from "./browser-reader-preferences.js";
import { ContentKitError, toContentKitError } from "./errors.js";
import type { ContentFilters } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** The host's account-owned preferred language, without a route fallback. */
export interface LanguagePreference {
  load(accountId: string): Promise<string | null | undefined>;
  save(language: string, accountId: string): Promise<void>;
}

export interface ReaderPreferenceState {
  language?: string;
  loading: boolean;
  saving: boolean;
  pending: boolean;
  error?: ContentKitError;
}

/** Guest storage and explicit, retryable registration inheritance. */
export class ReaderPreferencesClient {
  readonly browser;
  private viewer?: string | null;
  private adapter?: LanguagePreference;
  private accountLanguage?: string;
  private loading = false;
  private saving = false;
  private failure?: ContentKitError;
  private generation = 0;
  private syncing?: Promise<void>;
  private syncingGeneration = -1;
  private state: ReaderPreferenceState = Object.freeze({ loading: false, saving: false, pending: false });
  private readonly listeners = new Set<() => void>();
  private readonly identityListeners = new Set<() => void>();
  private stopBrowser?: () => void;
  private guestValue: string;

  constructor(private readonly http: Http) {
    this.browser = http.browserPreferences;
    this.guestValue = JSON.stringify(this.browser.snapshot());
  }

  readonly snapshot = (): ReaderPreferenceState => this.state;
  /** Current authenticated subject, null for a guest, undefined while unresolved. */
  readonly identity = (): string | null | undefined => this.viewer;
  readonly subscribeIdentity = (listener: () => void): (() => void) => {
    this.identityListeners.add(listener);
    return () => void this.identityListeners.delete(listener);
  };
  readonly subscribe = (listener: () => void): (() => void) => {
    if (!this.listeners.size) {
      this.stopBrowser = this.browser.subscribe(this.browserChanged);
      if (typeof window !== "undefined") window.addEventListener("online", this.retryOnline);
    }
    this.listeners.add(listener);
    this.refresh();
    return () => {
      this.listeners.delete(listener);
      if (!this.listeners.size) {
        this.stopBrowser?.();
        if (typeof window !== "undefined") window.removeEventListener("online", this.retryOnline);
      }
    };
  };

  configure(viewer: string | null | undefined, adapter?: LanguagePreference): void {
    this.http.setViewer(viewer);
    if (viewer === this.viewer && adapter === this.adapter) return;
    const identityChanged = viewer !== this.viewer;
    this.viewer = viewer;
    this.adapter = adapter;
    if (identityChanged) for (const listener of this.identityListeners) listener();
    const generation = ++this.generation;
    this.accountLanguage = undefined;
    this.failure = undefined;
    this.saving = false;
    this.loading = !!viewer && !!adapter;
    this.refresh();
    if (viewer) void this.activate(generation).catch(() => {});
  }

  /** Save an anonymous edit atomically after checking the current domain's wire limits. */
  setGuestFilters(filters: ReaderFilters): void {
    if (this.viewer !== null) throw new ContentKitError("invalid_request", "Guest preferences are only available while signed out.");
    this.browser.setFilters(filters,next=>this.http.validateGuestPreferences(next));
  }

  async setLanguage(language: string): Promise<void> {
    try { language = Intl.getCanonicalLocales(language)[0]!; }
    catch { throw new ContentKitError("invalid_request", "Invalid preferred language."); }
    if (!language) throw new ContentKitError("invalid_request", "Invalid preferred language.");
    const account = this.viewer;
    if (account == null) { this.browser.setLanguage(language); this.refresh(); return; }
    if (!this.adapter) throw new Error("contentkit: account language preference adapter is required");
    const generation = this.generation;
    this.saving = true;
    this.failure = undefined;
    this.refresh();
    try {
      await this.adapter.save(language, account);
      if (generation !== this.generation) throw new ContentKitError("aborted", "The account changed.");
      this.accountLanguage = language;
      this.browser.complete(account, "language");
    } catch (error) {
      if (generation === this.generation) this.failure = toContentKitError(error);
      throw error;
    } finally {
      if (generation === this.generation) { this.saving = false; this.refresh(); }
    }
  }

  /** Call only after a confirmed registration event, never on ordinary login. */
  async inheritAfterRegistration(account: string): Promise<void> {
    this.browser.register(account);
    this.refresh();
    return this.viewer === account ? this.retryInheritance() : Promise.resolve();
  }

  async retry(): Promise<void> {
    if (!this.viewer) return;
    const generation = this.generation;
    this.failure = undefined;
    this.loading = true;
    this.refresh();
    await this.activate(generation);
    if (generation === this.generation && this.failure) throw this.failure;
  }

  retryInheritance(): Promise<void> {
    if (!this.viewer || !this.browser.pending(this.viewer)) return Promise.resolve();
    if (this.syncing && this.syncingGeneration === this.generation) return this.syncing;
    const account = this.viewer, generation = this.generation;
    const task = this.inherit(account, generation).finally(() => {
      if (this.syncing === task) this.syncing = undefined;
      this.refresh();
    });
    this.syncing = task;
    this.syncingGeneration = generation;
    this.refresh();
    return this.syncing;
  }

  private async activate(generation: number): Promise<void> {
    try {
      await this.retryInheritance();
      if (generation !== this.generation) return;
      const language = this.viewer ? await this.adapter?.load(this.viewer) : undefined;
      if (generation === this.generation) this.accountLanguage = language ?? undefined;
    } catch (error) {
      if (generation === this.generation) this.failure = toContentKitError(error);
    } finally {
      if (generation === this.generation) { this.loading = false; this.refresh(); }
    }
  }

  private async inherit(account: string, generation: number): Promise<void> {
    this.failure = undefined;
    try {
      let pending = this.browser.pending(account)!;
      if (!pending.languageDone) {
        if (!this.adapter) throw new Error("contentkit: account language preference adapter is required");
        const language = (await this.adapter.load(account)) ?? null;
        if (generation !== this.generation) return;
        const live = this.browser.pending(account);
        if (live && !live.languageDone) {
          if (!live.languageObserved) this.browser.observeLanguage(account, language);
          pending = this.browser.pending(account)!;
          // The initial registration default may be non-null. A later different
          // choice wins over a retry of the captured guest preference.
          const changedLater = language !== pending.initialLanguage && language !== pending.language;
          if (!changedLater && language !== pending.language && pending.language) await this.adapter.save(pending.language, account);
          if (generation !== this.generation) return;
          this.accountLanguage = changedLater ? language ?? undefined : pending.language;
          this.browser.complete(account, "language");
        }
      }
      pending = this.browser.pending(account)!;
      if (pending && !pending.filtersDone) {
        if (generation !== this.generation) return;
        // Registration initializes account-wide choices. Only the server knows
        // which saved entities are toggleable on another domain or now stale.
        await call<"POST", "/me/content-filters/initialize", ContentFilters>(this.http, "POST", "/me/content-filters/initialize", {
          body: { account_id: account, filters: [...pending.filters] },
        });
        if (generation !== this.generation) return;
        this.browser.complete(account, "filters");
      }
      if (generation === this.generation) this.http.captureChanges()({ type: "contentFilters.changed" });
    } catch (error) {
      if (generation === this.generation) this.failure = toContentKitError(error);
      throw error;
    }
  }

  private readonly retryOnline = (): void => {
    if (this.state.pending || this.state.error) void this.retry().catch(() => {});
  };
  private readonly browserChanged = (): void => {
    const value = JSON.stringify(this.browser.snapshot());
    if (this.viewer === null && value !== this.guestValue) this.http.captureChanges()({ type: "contentFilters.changed" });
    this.guestValue = value;
    this.refresh();
  };

  private refresh(): void {
    const next: ReaderPreferenceState = {
      language: this.viewer == null ? this.browser.snapshot().language : this.accountLanguage,
      loading: this.loading, saving: this.saving,
      pending: !!this.viewer && !!this.browser.pending(this.viewer), error: this.failure,
    };
    if (next.language === this.state.language && next.loading === this.state.loading && next.saving === this.state.saving && next.pending === this.state.pending && next.error === this.state.error) return;
    this.state = Object.freeze(next);
    for (const listener of this.listeners) { try { listener(); } catch { /* Observers do not own persistence. */ } }
  }
}

import type { ContentKitClient } from "./client.js";
import { toContentKitError } from "./errors.js";
import type { ActivityContext, ActivityTarget, ExposureInput, SignalInput } from "./generated/wire.js";

export interface BeaconOptions {
  /** A signed-out visitor identity, persistent across this browser's tabs. */
  session?: () => string | undefined;
  /** How long events gather before they are sent, ms. Default 2000. */
  delay?: number;
  /** How long beacons stay off after the site records no signals, ms, before the content model is read again. Default 60000. */
  recheck?: number;
}

/** Where a click came from: the list render it was shown in. */
export interface Attribution {
  /** Language of the displayed content; defaults to the client's request language. */
  language?: string;
  render_id?: string;
  surface?: string;
  /** 1-based position in that render. */
  position?: number;
}


const MAX = 100;
// Fetch's keepalive limit includes all in-flight bodies in the fetch group.
// Leave headroom and serialize this module's keepalive requests across clients.
const KEEPALIVE_BYTES = 60 * 1024;
const FLUSH_BYTES = 48 * 1024;
let keepaliveTail: Promise<void> = Promise.resolve();
const encoder = new TextEncoder();
const bytes = (field: "signals" | "exposures", rows: readonly unknown[], session?: string) => encoder.encode(JSON.stringify({ session, [field]: rows })).length;

function packets<T>(items: readonly T[], field: "signals" | "exposures", session?: string): { items: T[]; bytes: number }[] {
  const out: { items: T[]; bytes: number }[] = [];
  let current: T[] = [];
  for (const item of items) {
    if (current.length && (current.length === MAX || bytes(field, [...current, item], session) > KEEPALIVE_BYTES)) {
      out.push({ items: current, bytes: bytes(field, current, session) });
      current = [];
    }
    current.push(item);
  }
  if (current.length) out.push({ items: current, bytes: bytes(field, current, session) });
  return out;
}

function serializeKeepalive(send: () => Promise<void>): Promise<void> {
  const pending = keepaliveTail.then(send, send);
  keepaliveTail = pending.then(() => {}, () => {});
  return pending;
}
const SESSION_KEY = "contentkit.visitor.v1";
export const eventId = () => (typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`);

let unstored: string | undefined;

/** A random signed-out browser identity shared across tabs/reloads, never an account credential. */
export function readerSession(): string {
  try {
    const kept = localStorage.getItem(SESSION_KEY);
    if (kept) return kept;
    const made = eventId();
    localStorage.setItem(SESSION_KEY, made);
    return made;
  } catch {
    return (unstored ??= eventId());
  }
}

/**
 * Batched, best-effort signals and exposures: gathered for `delay`, sent in
 * one request each, flushed with keepalive when the page hides. While the
 * content model says the site records no signals (or a send is refused as
 * not_configured or unavailable) events are dropped; after `recheck` the
 * next event reads the model again, so signals resume when the site's
 * signal plane is back.
 */
export class Beacons {
  private signals: SignalInput[] = [];
  private exposures: ExposureInput[] = [];
  private timer: ReturnType<typeof setTimeout> | undefined;
  private offUntil = 0;
  private recorded: Promise<boolean> | undefined;
  private generation = 0;
  private hidden = false;
  private readonly pending = new Set<Promise<void>>();
  private readonly exposureContexts = new Map<string, { active: boolean; releaseRevision: number; occurredAt?: string; contexts: Map<string, Promise<Receipt | null>> }>();
  private readonly session: () => string | undefined;
  private readonly delay: number;
  private readonly recheck: number;

  constructor(
    private readonly client: Pick<ContentKitClient, "discovery" | "kinds"> & Partial<Pick<ContentKitClient, "preferences">>,
    o: BeaconOptions = {},
  ) {
    this.session = o.session ?? readerSession;
    this.delay = o.delay ?? 2000;
    this.recheck = o.recheck ?? 60_000;
    client.preferences?.subscribeIdentity(() => {
      // The old credentials can no longer be assumed available for a flush.
      this.generation++;
      clearTimeout(this.timer);
      this.timer = undefined;
      this.signals = [];
      this.exposures = [];
      this.pending.clear();
      for (const state of this.exposureContexts.values()) state.active = false;
      this.exposureContexts.clear();
      this.recorded = undefined;
    });
    if (typeof addEventListener === "function") {
      addEventListener("pagehide", () => { this.hidden = true; void this.flush(true); });
      addEventListener("pageshow", () => { this.hidden = false; });
      addEventListener("visibilitychange", () => {
        this.hidden = typeof document !== "undefined" && document.visibilityState === "hidden";
        if (this.hidden) void this.flush(true);
      });
    }
  }

  /** A click on a listed card. Acquire its access snapshot at observation time. */
  click(content: string, a: Attribution & { version_id?: string } = {}, event = eventId()): void {
    if (this.paused()) return;
    const generation = this.generation;
    const at = new Date().toISOString();
    const { language, ...attribution } = a;
    const target = { content_id: content, version_id: a.version_id };
    this.track(this.contexts([target], generation, language).then((receipts) => {
      const receipt = receipts?.get(targetKey(target));
      if (!receipt || generation !== this.generation) return;
      this.signal({ content_id: content, type: "click", event_id: event, ...attribution,
        occurred_at: receipt.context?.occurred_at ?? at, activity_token: receipt.context?.token });
    }), generation);
  }

  /** A cumulative result-list stage; each newly observed target gets one receipt. */
  exposure(e: ExposureInput): void {
    if (this.paused()) return;
    const generation = this.generation;
    if (!e.shown.length) {
      if (e.stage !== "rendered" || e.surface !== "search" || !e.query_id || !e.search_query?.trim()) return;
      this.track(this.recording().then((enabled) => {
        if (!enabled || generation !== this.generation || this.paused()) return;
        this.queueExposure({ ...e, occurred_at: e.occurred_at ?? new Date().toISOString() });
      }), generation);
      return;
    }
    const key = `${e.render_id}\u0000${e.stage}`;
    let state = this.exposureContexts.get(key);
    if (!state) {
      state = { active: true, releaseRevision: 0, contexts: new Map() };
      this.exposureContexts.set(key, state);
    }
    state.releaseRevision++;
    const captured = state;
    const unseen = e.shown.filter((p) => !captured.contexts.has(targetKey(p)));
    if (unseen.length) {
      const contexts = this.contexts(unseen, generation, e.language);
      for (const target of unseen) {
        const key = targetKey(target);
        const pending: Promise<Receipt | null> = contexts.then((values) => {
          const receipt = values?.get(key) ?? null;
          if (!receipt && captured.contexts.get(key) === pending) captured.contexts.delete(key);
          return receipt;
        }, (error: unknown) => {
          if (captured.contexts.get(key) === pending) captured.contexts.delete(key);
          throw error;
        });
        captured.contexts.set(key, pending);
      }
    }
    this.track(Promise.all(e.shown.map((p) => captured.contexts.get(targetKey(p))!)).then((receipts) => {
      if (generation !== this.generation || !captured.active || receipts.some((r) => !r)) return;
      const valid = receipts as Receipt[];
      if (new Set(valid.map((r) => r.enabled)).size !== 1) return;
      captured.occurredAt ??= valid.find((r) => r.context)?.context?.occurred_at ?? e.occurred_at ?? new Date().toISOString();
      const enriched = { ...e, occurred_at: captured.occurredAt,
        shown: e.shown.map((p, i) => ({ ...p, activity_token: valid[i]?.context?.token })) };
      this.queueExposure(enriched);
    }), generation);
  }

  private queueExposure(e: ExposureInput): void {
    // A later cumulative revision supersedes a queued earlier snapshot.
    const previous = this.exposures.find((x) => x.render_id === e.render_id && x.stage === e.stage);
    if (previous && (previous.revision ?? 0) > (e.revision ?? 0)) return;
    this.exposures = this.exposures.filter((x) => x.render_id !== e.render_id || x.stage !== e.stage);
    this.exposures.push(e);
    this.schedule();
  }

  /** Keep receipt state when a mounted list resumes (including StrictMode replay). */
  beginExposure(render: string): void {
    for (const stage of ["served", "rendered", "visible"]) {
      const state = this.exposureContexts.get(`${render}\u0000${stage}`);
      if (state) state.releaseRevision++;
    }
  }

  /** Retire receipt state after a list unmounts without cancelling observations. */
  endExposure(render: string): void {
    for (const stage of ["served", "rendered", "visible"]) {
      const key = `${render}\u0000${stage}`, state = this.exposureContexts.get(key);
      if (!state) continue;
      const release = ++state.releaseRevision;
      queueMicrotask(() => {
        if (this.exposureContexts.get(key) === state && state.releaseRevision === release) this.exposureContexts.delete(key);
      });
    }
  }

  /** Sends ready observations; receipts are never acquired for the first time here. */
  async flush(keepalive = false): Promise<void> {
    clearTimeout(this.timer);
    this.timer = undefined;
    const generation = this.generation;
    await Promise.allSettled([...this.pending]);
    if (generation !== this.generation) return;
    clearTimeout(this.timer);
    this.timer = undefined;
    const signals = this.signals.splice(0), exposures = this.exposures.splice(0);
    if (this.paused() || (!signals.length && !exposures.length)) return;
    const session = this.session();
    const jobs = [
      ...packets(signals, "signals", session).map((packet) => ({ ...packet, send: (persist: boolean) => this.client.discovery.signals(packet.items, session, { keepalive: persist }) })),
      ...packets(exposures, "exposures", session).map((packet) => ({ ...packet, send: (persist: boolean) => this.client.discovery.exposures(packet.items, session, { keepalive: persist }) })),
    ];
    const done = await Promise.allSettled(jobs.map((job) => {
      const persist = keepalive && job.bytes <= KEEPALIVE_BYTES;
      const send = () => generation === this.generation ? job.send(persist) : Promise.resolve();
      // A single oversized cumulative exposure cannot be split without losing
      // history. Normal fetch is the best effort available during pagehide.
      return persist ? serializeKeepalive(send) : send();
    }));
    if (generation === this.generation && done.some((r) => r.status === "rejected" && unavailable(r.reason))) this.pause();
  }

  private async contexts(targets: readonly ActivityTarget[], generation: number, language?: string): Promise<Map<string, Receipt> | null> {
    if (this.paused() || !(await this.recording())) {
      if (generation === this.generation) this.pause();
      return null;
    }
    if (generation !== this.generation) return null;
    // Exposure rows carry list fields (position); the strict endpoint takes only the target.
    const unique = [...new Map(targets.map((t) => [targetKey(t), { content_id: t.content_id, version_id: t.version_id }])).values()];
    const batches = [];
    const session = this.session();
    for (let i = 0; i < unique.length; i += MAX) batches.push(this.client.discovery.activityContexts(unique.slice(i, i + MAX), session, language));
    const responses = await Promise.all(batches);
    if (generation !== this.generation) return null;
    const out = new Map<string, Receipt>();
    for (let i = 0; i < responses.length; i++) {
      const response = responses[i]!;
      if (typeof response?.enabled !== "boolean" || (response.enabled && !Array.isArray(response.contexts))) throw new Error("contentkit: invalid activity context response");
      const contexts = new Map((response.contexts ?? []).map((context) => [targetKey(context), context]));
      for (const target of unique.slice(i * MAX, (i + 1) * MAX)) {
        const context = contexts.get(targetKey(target));
        if (response.enabled && (!context?.token || !Number.isFinite(Date.parse(context.occurred_at)))) throw new Error("contentkit: invalid activity context");
        out.set(targetKey(target), { enabled: response.enabled, context: response.enabled ? context : undefined });
      }
    }
    return out;
  }

  private track(task: Promise<void>, generation: number): void {
    const pending = task.catch((error) => { if (generation === this.generation && unavailable(error)) this.pause(); }).finally(() => { this.pending.delete(pending); });
    this.pending.add(pending);
  }

  private signal(s: SignalInput): void {
    if (this.paused()) return;
    this.signals.push(s);
    this.schedule();
  }
  private paused(): boolean {
    if (this.offUntil > Date.now()) return true;
    this.offUntil = 0;
    return false;
  }
  private pause(): void { this.offUntil = Date.now() + this.recheck; this.recorded = undefined; }
  private recording(): Promise<boolean> {
    return (this.recorded ??= this.client.kinds().then((m) => m.signals, () => true));
  }
  private schedule(): void {
    const queued = bytes("signals", this.signals, this.session()) + bytes("exposures", this.exposures, this.session());
    if (this.signals.length + this.exposures.length >= MAX || queued >= FLUSH_BYTES) return void this.flush(this.hidden);
    this.timer ??= setTimeout(() => void this.flush(), this.delay);
  }
}

type Receipt = { enabled: boolean; context?: ActivityContext };
const targetKey = (target: ActivityTarget) => `${target.content_id}\u0000${target.version_id ?? ""}`;
const unavailable = (error: unknown) => ["not_configured", "unavailable", "server_unavailable"].includes(toContentKitError(error).code);

import type { ContentKitClient } from "./client.js";
import { toContentKitError } from "./errors.js";
import type { ActivityEvent } from "./generated/wire.js";

/** The list an open came from: its response's request_id, surface and 1-based position. */
export interface OpenSource {
  requestId?: string;
  surface?: string;
  position?: number;
}

export interface ActivityReporterOptions {
  /** How long events gather before they are sent, ms. Default 1000. */
  delay?: number;
  /** How long reports stay off after the site logs no activity, ms, before the content model is read again. Default 60000. */
  recheck?: number;
}

/** The server's event bound per report (discovery.MaxActivityEvents). */
const MAX_EVENTS = 50;
/** An open of the same content this soon after another is the same open (a card click, then its page). */
const OPEN_WINDOW = 30_000;
/** The server's bound on one progress report's active time. */
const MAX_ACTIVE_MS = 6 * 60 * 1000;

/**
 * One client's activity: opens and progress reports, gathered for `delay`
 * and sent as POST /v1/activity, with keepalive when the page hides. While
 * the content model says the site logs no activity (or a send answers
 * not_configured) events are dropped; after `recheck` the model is read
 * again. An identity change drops what the old identity queued.
 */
export class ActivityReporter {
  private queue: ActivityEvent[] = [];
  private timer: ReturnType<typeof setTimeout> | undefined;
  private offUntil = 0;
  private enabled: Promise<boolean> | undefined;
  private generation = 0;
  private readonly opened = new Map<string, number>();
  private readonly delay: number;
  private readonly recheck: number;

  constructor(
    private readonly client: Pick<ContentKitClient, "discovery" | "kinds"> & Partial<Pick<ContentKitClient, "preferences">>,
    o: ActivityReporterOptions = {},
    private readonly now: () => number = Date.now,
  ) {
    this.delay = o.delay ?? 1000;
    this.recheck = o.recheck ?? 60_000;
    client.preferences?.subscribeIdentity(() => {
      this.generation++;
      clearTimeout(this.timer);
      this.timer = undefined;
      this.queue = [];
      this.opened.clear();
    });
    if (typeof addEventListener === "function") {
      addEventListener("pagehide", () => void this.flush(true));
      addEventListener("visibilitychange", () => {
        if (typeof document !== "undefined" && document.visibilityState === "hidden") void this.flush(true);
      });
    }
  }

  /** The reader opened a content item, from a list (source) or from outside. */
  open(contentId: string, source: OpenSource = {}): void {
    const at = this.now(), last = this.opened.get(contentId);
    if (last !== undefined && at - last < OPEN_WINDOW) return;
    this.opened.set(contentId, at);
    const event: ActivityEvent = { type: "open", content_id: contentId };
    if (source.requestId) event.request_id = source.requestId;
    if (source.surface) event.surface = source.surface;
    if (source.position && source.position > 0) event.position = Math.floor(source.position);
    this.add(event);
  }

  /** A progress report; ProgressTracker sends these. */
  progress(contentId: string, p: Omit<ActivityEvent, "type" | "content_id">, keepalive = false): void {
    this.add({ type: "progress", content_id: contentId, ...p, active_ms: Math.min(MAX_ACTIVE_MS, Math.max(0, Math.round(p.active_ms ?? 0))) });
    if (keepalive) void this.flush(true);
  }

  /** Sends what is queued now. */
  async flush(keepalive = false): Promise<void> {
    clearTimeout(this.timer);
    this.timer = undefined;
    const generation = this.generation;
    const events = this.queue.splice(0);
    if (!events.length || this.paused() || !(await this.recording()) || generation !== this.generation) return;
    const sends: Promise<void>[] = [];
    for (let i = 0; i < events.length; i += MAX_EVENTS) sends.push(this.client.discovery.activity(events.slice(i, i + MAX_EVENTS), { keepalive }));
    const done = await Promise.allSettled(sends);
    if (done.some((r) => r.status === "rejected" && unavailable(r.reason))) this.pause();
  }

  private add(event: ActivityEvent): void {
    if (this.paused()) return;
    this.queue.push(event);
    if (this.queue.length >= MAX_EVENTS) return void this.flush();
    this.timer ??= setTimeout(() => void this.flush(), this.delay);
  }

  private paused(): boolean {
    if (this.offUntil > this.now()) return true;
    this.offUntil = 0;
    return false;
  }

  private pause(): void {
    this.offUntil = this.now() + this.recheck;
    this.enabled = undefined;
  }

  private recording(): Promise<boolean> {
    return (this.enabled ??= this.client.kinds().then((m) => m.signals, () => true).then((on) => {
      if (!on) this.pause();
      return on;
    }));
  }
}

const unavailable = (error: unknown) => ["not_configured", "unavailable", "server_unavailable"].includes(toContentKitError(error).code);

export interface ProgressTrackerOptions {
  contentId: string;
  /** Receives each report; keepalive when the page is leaving. */
  report: (p: { active_ms: number; furthest: number; current: number; length: number }, keepalive: boolean) => void;
  /** Without input for this long the reader is idle; default 60 seconds. Playback keeps them active. */
  idleAfter?: number;
  /** Active time between reports while reading; default 5 minutes. */
  every?: number;
  now?: () => number;
}

/**
 * One reading of a content item: active time (a visible tab and genuine
 * input or playback), the furthest and current page or second and the
 * length. It reports every `every` of active time and on leave, and skips a
 * report when nothing changed since the last one.
 */
export class ProgressTracker {
  private readonly now: () => number;
  private readonly idleAfter: number;
  private readonly every: number;
  private last: number;
  private input: number;
  private visible = true;
  private playing = false;
  private active = 0;
  private furthest = 0;
  private current = 0;
  private length = 0;
  private sent = { furthest: 0, current: 0 };
  private stopped = false;

  constructor(private readonly o: ProgressTrackerOptions) {
    this.now = o.now ?? (() => (typeof performance === "undefined" ? Date.now() : performance.now()));
    this.idleAfter = o.idleAfter ?? 60_000;
    this.every = o.every ?? 5 * 60_000;
    this.last = this.input = this.now();
  }

  /** Genuine input: the reader is active again. */
  activity(): void {
    this.advance();
    this.input = this.now();
  }

  /** The tab became visible or hidden; hiding reports with keepalive. */
  setVisible(visible: boolean): void {
    this.advance();
    this.visible = visible;
    if (visible) this.input = this.now();
    else this.leave();
  }

  /** Playback started or stopped; playing counts as active. */
  setPlaying(playing: boolean): void {
    this.advance();
    this.playing = playing;
    if (!playing) this.input = this.now();
  }

  /** The current page (1-based) or second, and the length in the same unit when known. */
  point(current: number, length?: number): void {
    this.advance();
    if (length !== undefined && Number.isFinite(length) && length > 0) this.length = Math.floor(length);
    if (!Number.isFinite(current) || current < 0) return;
    this.current = Math.floor(current);
    this.furthest = Math.max(this.furthest, this.current);
  }

  /** Advances the clock; a timer calls it, and it reports once `every` of active time gathered. */
  tick(): void {
    this.advance();
    if (this.active >= this.every) this.report(false);
  }

  /** The reader is leaving: report what changed, with keepalive. */
  leave(): void {
    this.advance();
    this.report(true);
  }

  /** Ends the reading with a last report. */
  stop(): void {
    if (this.stopped) return;
    this.leave();
    this.stopped = true;
  }

  /** Active milliseconds not reported yet. */
  pending(): number {
    this.advance();
    return this.active;
  }

  private advance(): void {
    const at = this.now();
    if (this.visible && !this.stopped) {
      // Without playback, attention ends idleAfter past the last input.
      const end = this.playing ? at : Math.min(at, this.input + this.idleAfter);
      if (end > this.last) this.active += end - this.last;
    }
    this.last = at;
  }

  private report(keepalive: boolean): void {
    if (this.stopped) return;
    const known = this.length > 0;
    const furthest = known ? Math.min(this.furthest, this.length) : 0, current = known ? Math.min(this.current, this.length) : 0;
    if (this.active < 1000 && furthest === this.sent.furthest && current === this.sent.current) return;
    const active = Math.round(this.active);
    this.active = 0;
    this.sent = { furthest, current };
    this.o.report({ active_ms: active, furthest, current, length: known ? this.length : 0 }, keepalive);
  }
}

import type { ActivityContext, CoverageRange, EngagementSnapshot, PageEngagement, WatchedRange } from "./generated/wire.js";

export interface VisiblePage {
  /** Zero-based index in the version's reading order. */
  index: number;
  /** The part actually on screen, in 0..10000 basis points of page height. */
  ranges: readonly CoverageRange[];
}

export interface ReadingObservation {
  pages?: readonly VisiblePage[];
  video?: { positionMs: number; playing: boolean; seeking?: boolean };
  resume?: string;
}

export interface EngagementTrackerOptions {
  contentId: string;
  versionId?: string;
  visitId?: string;
  originClickId?: string;
  /** Acquired when attention begins, never by preloading or opening a route. */
  begin: () => Promise<ActivityContext | null>;
  submit: (snapshot: EngagementSnapshot, options: { keepalive: boolean }) => Promise<void>;
  onSnapshot?: (snapshot: EngagementSnapshot) => void;
  idleAfter?: number;
  checkpointInterval?: number;
  now?: () => number;
  id?: () => string;
}

const MAX_SEGMENT = 120_000;
const MAX_PENDING = 100;
const clock = () => typeof performance === "undefined" ? Date.now() : performance.now();
const randomId = () => crypto.randomUUID();
const durationOption = (value: number | undefined, fallback: number, max = Infinity) =>
  value !== undefined && Number.isFinite(value) ? Math.max(1000, Math.min(max, Math.floor(value))) : fallback;

/** Coalesce coverage, so looking at the same page or video interval never inflates completion. */
function union(ranges: readonly CoverageRange[], max = Infinity): CoverageRange[] {
  const ordered = ranges.filter((range) => Number.isFinite(range.from) && Number.isFinite(range.to))
    .map(({ from, to }) => ({ from: Math.max(0, Math.round(from)), to: Math.min(max, Math.round(to)) }))
    .filter(({ from, to }) => to > from).sort((a, b) => a.from - b.from);
  const merged: CoverageRange[] = [];
  for (const range of ordered) {
    const last = merged.at(-1);
    if (last && range.from <= last.to) last.to = Math.max(last.to, range.to);
    else merged.push({ ...range });
  }
  return merged;
}

interface Segment {
  id: string;
  start: number;
  active: number;
  pages: Map<number, PageEngagement>;
  watched: CoverageRange[];
  revision: number;
  changed: boolean;
  resume?: string;
  receipt: Promise<ActivityContext | null>;
}

/**
 * One content visit, represented by cumulative, bounded attention snapshots.
 * The clock and transport are injected so ownership and browser lifecycle stay
 * outside the accounting. Repeated snapshots replace a revision, not add views.
 */
export class EngagementTracker {
  readonly visitId: string;
  private readonly now: () => number;
  private readonly id: () => string;
  private readonly idleAfter: number;
  private readonly interval: number;
  private last: number;
  private lastActivity: number;
  private attentive = false;
  private observation: ReadingObservation = {};
  private segment?: Segment;
  private pending: { snapshot: EngagementSnapshot; receipt: Promise<ActivityContext | null> }[] = [];
  private tail: Promise<void> = Promise.resolve();
  private stopped = false;
  private keepaliveRequested = false;

  constructor(private readonly options: EngagementTrackerOptions) {
    this.now = options.now ?? clock;
    this.id = options.id ?? randomId;
    this.visitId = options.visitId ?? this.id();
    this.idleAfter = durationOption(options.idleAfter, 60_000);
    this.interval = durationOption(options.checkpointInterval, MAX_SEGMENT, MAX_SEGMENT);
    this.last = this.lastActivity = this.now();
  }

  /** Genuine input extends the reading window; a timer or preload does not. */
  activity(): void {
    this.advance(this.now());
    this.lastActivity = this.now();
    this.startObservedSegment();
  }

  attention(attentive: boolean): void {
    this.advance(this.now());
    this.attentive = attentive;
    this.startObservedSegment();
  }

  observe(observation: ReadingObservation): void {
    const at = this.now();
    this.advance(at, observation.video);
    let remaining = 512;
    const pages = new Map<number, VisiblePage>();
    for (const page of observation.pages ?? []) {
      if (!Number.isSafeInteger(page.index) || page.index < 0 || pages.size >= 128 || !remaining) continue;
      const ranges = union(page.ranges, 10_000).slice(0, Math.min(16, remaining));
      if (!ranges.length) continue;
      pages.set(page.index, { index: page.index, ranges });
      remaining -= ranges.length;
    }
    this.observation = { ...observation, pages: [...pages.values()] };
    this.startObservedSegment();
    if (observation.resume !== undefined && this.segment) {
      this.segment.resume = observation.resume;
      this.segment.changed = true;
    }
  }

  /** Tick even while the geometry is unchanged, so idle and checkpoint bounds hold. */
  tick(): void { this.advance(this.now()); this.startObservedSegment(); }

  /** Send accumulated attention; failed packets retain their exact revision for retry. */
  flush(keepalive = false): Promise<void> {
    if (this.stopped) return Promise.resolve();
    this.advance(this.now());
    this.capture();
    return this.send(keepalive);
  }

  /** Account changes discard old ownership instead of sending under new credentials. */
  discard(): void {
    this.stopped = true;
    this.segment = undefined;
    this.pending = [];
    this.observation = {};
  }

  private startObservedSegment(): void {
    const at = this.now();
    if (!this.stopped && this.attentive && (this.observation.video?.playing
      || (this.observation.pages?.length && at < this.lastActivity + this.idleAfter))) this.segment ??= this.createSegment(at);
  }

  private receipt(): Promise<ActivityContext | null> {
    try { return this.options.begin().catch(() => null); }
    catch { return Promise.resolve(null); }
  }

  private createSegment(start: number): Segment {
    return {
      id: this.id(), start, active: 0, pages: new Map(), watched: [], revision: 0, changed: false,
      resume: this.observation.resume,
      // Capture immediately; a later checkpoint must not rebind old attention
      // to a new access level. An unavailable receipt drops remote telemetry.
      receipt: this.receipt(),
    };
  }

  private advance(at: number, video?: ReadingObservation["video"]): void {
    if (this.stopped) return;
    const previous = this.last;
    this.last = Math.max(previous, at);
    if (at - previous > 5000) {
      // A suspended tab or stalled timer cannot tell us what stayed visible or
      // which access cohort applied. Never backfill it under a new receipt.
      this.capture(); this.segment = undefined; void this.send(false);
      return;
    }
    this.record(previous, at, video, at);
    if (this.segment && at >= this.segment.start + this.interval) {
      this.capture(); this.segment = undefined; void this.send(false);
    }
  }

  private record(from: number, to: number, video: ReadingObservation["video"], sampledAt: number): void {
    const pages = this.observation.pages ?? [];
    const previousVideo = this.observation.video;
    const duration = sampledAt - from;
    const delta = video && previousVideo ? video.positionMs - previousVideo.positionMs : 0;
    const playing = previousVideo?.playing && !previousVideo.seeking && video && !video.seeking
      && delta > 0 && duration > 0 && delta <= duration * 4 + 250;
    if (!this.attentive || (!pages.length && !playing)) return;
    const end = playing ? to : Math.min(to, this.lastActivity + this.idleAfter);
    let start = from;
    while (end > start) {
      this.segment ??= this.createSegment(start);
      const boundary = Math.min(end, this.segment.start + this.interval);
      const elapsed = boundary - start;
      if (elapsed <= 0) {
        this.capture(); this.segment = undefined; void this.send(false);
        continue;
      }
      // Real playback can be faster than 1x; discontinuous seeks are not watched.
      const watched = playing && previousVideo ? {
        from: previousVideo.positionMs + delta * (start - from) / duration,
        to: previousVideo.positionMs + delta * (boundary - from) / duration,
      } : undefined;
      const nextPages = new Map(this.segment.pages);
      for (const page of pages) nextPages.set(page.index, { index: page.index, active_ms: 0,
        ranges: union([...(nextPages.get(page.index)?.ranges ?? []), ...page.ranges], 10_000) });
      const nextWatched = watched ? union([...this.segment.watched, watched]) : this.segment.watched;
      if (this.segment.active > 0 && (nextPages.size > 128 || [...nextPages.values()].some((page) => page.ranges.length > 16)
        || [...nextPages.values()].reduce((total, page) => total + page.ranges.length, 0) > 512 || nextWatched.length > 256)) {
        this.capture(); this.segment = undefined; void this.send(false);
        continue;
      }
      this.segment.active += elapsed;
      this.segment.changed = true;
      this.segment.resume = this.observation.resume;
      if (pages.length) {
        const share = elapsed / pages.length;
        for (const page of pages) {
          let row = this.segment.pages.get(page.index);
          if (!row) this.segment.pages.set(page.index, (row = { index: page.index, ranges: [], active_ms: 0 }));
          row.active_ms += share;
          row.ranges = union([...row.ranges, ...page.ranges], 10_000);
        }
      }
      this.segment.watched = nextWatched;
      start = boundary;
      if (start >= this.segment.start + this.interval) {
        this.capture(); this.segment = undefined; void this.send(false);
      }
    }
  }

  private capture(): void {
    const segment = this.segment;
    if (!segment?.changed || segment.active < 1) return;
    const pages = [...segment.pages.values()].filter((page) => page.active_ms >= 1)
      .sort((a, b) => a.index - b.index).map((page) => ({ ...page, active_ms: Math.floor(page.active_ms), ranges: page.ranges.map((range) => ({ ...range })) }));
    const watched: WatchedRange[] = segment.watched.map(({ from, to }) => ({ from_ms: from, to_ms: to }));
    if (!pages.length && !watched.length) return;
    segment.changed = false;
    const snapshot: EngagementSnapshot = {
      content_id: this.options.contentId, version_id: this.options.versionId,
      visit_id: this.visitId, segment_id: segment.id, revision: ++segment.revision,
      ...(this.options.originClickId ? { origin_click_id: this.options.originClickId } : {}),
      activity_token: "", active_ms: Math.floor(segment.active),
      ...(segment.resume !== undefined ? { resume: segment.resume } : {}),
      ...(pages.length ? { pages } : {}), ...(watched.length ? { watched } : {}),
    };
    this.options.onSnapshot?.(snapshot);
    // A newer cumulative revision makes an unsent earlier revision unnecessary.
    this.pending = this.pending.filter((entry) => entry.snapshot.segment_id !== segment.id);
    this.pending.push({ snapshot, receipt: segment.receipt });
    if (this.pending.length > MAX_PENDING) this.pending.shift();
  }

  private send(keepalive: boolean): Promise<void> {
    this.keepaliveRequested ||= keepalive;
    this.tail = this.tail.then(async () => {
      for (const entry of [...this.pending]) {
        if (this.stopped) return;
        const context = await entry.receipt;
        if (this.stopped) return;
        if (!context) { this.pending = this.pending.filter((item) => item !== entry); continue; }
        try {
          await this.options.submit({ ...entry.snapshot, activity_token: context.token }, { keepalive: keepalive || this.keepaliveRequested });
          this.pending = this.pending.filter((item) => item !== entry);
        } catch {
          // A later checkpoint, online event or exit can retry this same revision.
        }
      }
      if (!this.pending.length) this.keepaliveRequested = false;
    });
    return this.tail;
  }
}

import type { DiscoveryClient } from "./discovery.js";
import { toContentKitError } from "./errors.js";
import type { ExposureInput, SignalInput } from "./generated/wire.js";

export interface BeaconOptions {
  /** A signed-out visitor's session; default one per browser tab. */
  session?: () => string | undefined;
  /** How long events gather before they are sent, ms. Default 2000. */
  delay?: number;
}

/** Where a click or a view came from: the list render it was shown in. */
export interface Attribution {
  render_id?: string;
  surface?: string;
  /** 1-based position in that render. */
  position?: number;
}

export interface ViewSession {
  /** Reports the view so far: pages read, seconds watched or percent scrolled; resume is where to pick up. */
  update(o: { progress?: number; duration_s?: number; resume?: string }): void;
}

const MAX = 100;
const SESSION_KEY = "contentkit.session";
export const eventId = () => (typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`);

let unstored: string | undefined;

function tabSession(): string {
  try {
    const kept = sessionStorage.getItem(SESSION_KEY);
    if (kept) return kept;
    const made = eventId();
    sessionStorage.setItem(SESSION_KEY, made);
    return made;
  } catch {
    return (unstored ??= eventId());
  }
}

/**
 * Batched, best-effort signals and exposures: gathered for `delay`, sent in
 * one request each, flushed with keepalive when the page hides. A site
 * without the signal plane (not_configured) turns them off.
 */
export class Beacons {
  private signals: SignalInput[] = [];
  private exposures: ExposureInput[] = [];
  private timer: ReturnType<typeof setTimeout> | undefined;
  private off = false;
  private readonly session: () => string | undefined;
  private readonly delay: number;

  constructor(
    private readonly discovery: DiscoveryClient,
    o: BeaconOptions = {},
  ) {
    this.session = o.session ?? tabSession;
    this.delay = o.delay ?? 2000;
    if (typeof addEventListener === "function") {
      addEventListener("pagehide", () => void this.flush(true));
      addEventListener("visibilitychange", () => {
        if (typeof document !== "undefined" && document.visibilityState === "hidden") void this.flush(true);
      });
    }
  }

  /** A click on a listed card. */
  click(content: string, a: Attribution & { version_id?: string } = {}): void {
    this.signal({ content_id: content, type: "click", event_id: eventId(), occurred_at: new Date().toISOString(), ...a });
  }

  /** A consumption session of a content (or one version): each update a revision of one event. */
  view(content: string, version?: string, a: Attribution = {}): ViewSession {
    const event = eventId();
    let revision = 0;
    return {
      update: (o) => this.signal({ content_id: content, version_id: version, type: "view", event_id: event, revision: ++revision, occurred_at: new Date().toISOString(), ...a, ...o }),
    };
  }

  /** A result list served, rendered or shown. */
  exposure(e: ExposureInput): void {
    if (this.off || !e.shown.length) return;
    this.exposures.push(e);
    this.schedule();
  }

  /** Sends what has gathered now. */
  async flush(keepalive = false): Promise<void> {
    clearTimeout(this.timer);
    this.timer = undefined;
    const signals = this.signals.splice(0);
    const exposures = this.exposures.splice(0);
    if (this.off) return;
    const session = this.session();
    const sends: Promise<void>[] = [];
    for (let i = 0; i < signals.length; i += MAX) sends.push(this.discovery.signals(signals.slice(i, i + MAX), session, { keepalive }));
    for (let i = 0; i < exposures.length; i += MAX) sends.push(this.discovery.exposures(exposures.slice(i, i + MAX), session, { keepalive }));
    const done = await Promise.allSettled(sends);
    if (done.some((r) => r.status === "rejected" && toContentKitError(r.reason).code === "not_configured")) this.off = true;
  }

  private signal(s: SignalInput): void {
    if (this.off) return;
    // A view's later revision replaces its earlier one still waiting.
    if (s.type === "view") this.signals = this.signals.filter((x) => x.event_id !== s.event_id);
    this.signals.push(s);
    this.schedule();
  }

  private schedule(): void {
    if (this.signals.length + this.exposures.length >= MAX) return void this.flush();
    this.timer ??= setTimeout(() => void this.flush(), this.delay);
  }
}

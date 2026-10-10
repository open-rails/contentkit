const MiB = 1 << 20;

export interface PacerOptions {
  minPartSize: number;
  maxPartSize: number;
  maxConcurrency: number;
  /** Seconds one part should take; keeps parts well inside proxy timeouts. */
  targetSeconds: number;
}

/**
 * Adapts multipart part size and concurrency to measured throughput. Until the
 * first part lands it sends one min-size part at a time, so a slow link never
 * starts several parts that each outlast the proxy timeout.
 */
export class Pacer {
  private rate?: number; // aggregate bytes/s, smoothed

  constructor(private readonly o: PacerOptions) {}

  /** A part of bytes took ms with inFlight parts running alongside it (itself included). */
  record(bytes: number, ms: number, inFlight: number): void {
    const agg = (bytes / Math.max(ms, 1)) * 1000 * Math.max(inFlight, 1);
    this.rate = this.rate === undefined ? agg : 0.5 * this.rate + 0.5 * agg;
  }

  get concurrency(): number {
    if (this.rate === undefined) return 1;
    const fit = Math.floor((this.rate * this.o.targetSeconds) / this.o.minPartSize);
    return clamp(fit, 1, this.o.maxConcurrency);
  }

  partSize(): number {
    if (this.rate === undefined) return this.o.minPartSize;
    const size = Math.floor(((this.rate / this.concurrency) * this.o.targetSeconds) / MiB) * MiB;
    return clamp(size, this.o.minPartSize, this.o.maxPartSize);
  }
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(Math.max(v, lo), hi);
}

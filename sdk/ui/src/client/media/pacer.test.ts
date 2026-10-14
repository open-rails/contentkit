import { describe, expect, it } from "vitest";
import { Pacer } from "./pacer.js";

const MiB = 1 << 20;
const opts = { minPartSize: 8 * MiB, maxPartSize: 16 * MiB, maxConcurrency: 4, targetSeconds: 30 };

describe("Pacer", () => {
  it("starts with one minimum part", () => {
    const p = new Pacer(opts);
    expect(p.concurrency).toBe(1);
    expect(p.partSize()).toBe(8 * MiB);
  });

  it("stays at one min-size part on a slow link", () => {
    const p = new Pacer(opts);
    p.record(8 * MiB, (8 * MiB) / 151, 1); // ~151 KB/s
    expect(p.concurrency).toBe(1);
    expect(p.partSize()).toBe(8 * MiB);
  });

  it("grows to max parts and concurrency on a fast link", () => {
    const p = new Pacer(opts);
    p.record(8 * MiB, 100, 1); // ~80 MiB/s
    expect(p.concurrency).toBe(4);
    expect(p.partSize()).toBe(16 * MiB);
  });

  it("sizes parts between the bounds in MiB steps", () => {
    const p = new Pacer({ ...opts, maxConcurrency: 1 });
    p.record(12 * MiB, 30_000, 1); // 12 MiB per 30 s
    expect(p.partSize()).toBe(12 * MiB);
  });
});

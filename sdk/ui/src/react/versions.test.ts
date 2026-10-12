import { expect, it } from "vitest";
import type { ReaderPlan } from "../client/generated/wire.js";
import { nextRelease } from "./versions.js";

const plan = (segments: Partial<ReaderPlan["segments"][number]>[]): ReaderPlan =>
  ({ content_id: "c", version_id: "v", publication_id: "p", pages: 0, previews: [], segments: segments.map((s) => ({ start: 0, end: 1, released: true, locked: false, ...s })) }) as ReaderPlan;

it("asks again at the next unreleased segment's release", () => {
  const now = Date.parse("2026-10-12T00:00:00Z");
  expect(nextRelease(plan([{ released: true }]), now)).toBeNull();
  expect(nextRelease(plan([{ released: false, release_at: "2026-10-13T00:00:00Z" }, { released: false, release_at: "2026-10-12T06:00:00Z" }]), now)).toBe(Date.parse("2026-10-12T06:00:00Z"));
  expect(nextRelease(plan([{ released: false, release_at: "2026-10-11T00:00:00Z" }]), now)).toBeNull();
  expect(nextRelease(null, now)).toBeNull();
});

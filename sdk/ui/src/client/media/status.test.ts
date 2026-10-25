import { expect, it } from "vitest";
import type { ReadResult } from "../generated/wire.js";
import { processing } from "./windows.js";

const read: ReadResult = { access: "full", expires: 0, total: 0, offset: 0, limit: 0, files: [], status: {
  state: "ready", failure_count: 0, uploads: { total: 0, completed: 0, failed: 0 }, archives: { total: 0, completed: 0, failed: 0 }, presets: [], failures: [],
} };

it("observes status-only pending work and stops at ready or capacity limits", () => {
  expect(processing(read)).toBe(false);
  expect(processing({ ...read, status: { ...read.status!, state: "processing" } })).toBe(true);
  expect(processing({ ...read, status: { ...read.status!, state: "full" } })).toBe(false);
});

it("uses the canonical whole-item status when file metadata predates a stream update", () => {
  const files = [{ path: "pages/new.png", upload: true, unattached: true, type: "image/png", pending: ["low"] }];
  expect(processing({ ...read, files })).toBe(false);
  expect(processing({ ...read, files, status: { ...read.status!, state: "processing" } })).toBe(true);
  expect(processing({ ...read, files, status: undefined })).toBe(true);
});

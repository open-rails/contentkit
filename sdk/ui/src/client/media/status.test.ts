import { expect, it } from "vitest";
import type { ReadResult } from "../generated/wire.js";
import { processing } from "./windows.js";

const read: ReadResult = { access: "full", expires: 0, total: 0, offset: 0, limit: 0, files: [], status: {
  state: "ready", failure_count: 0, uploads: { total: 0, completed: 0, failed: 0 }, archives: { total: 0, completed: 0, failed: 0 }, presets: [], failures: [],
} };

it("polls status-only pending work and stops at ready or capacity limits", () => {
  expect(processing(read)).toBe(false);
  expect(processing({ ...read, status: { ...read.status!, state: "processing" } })).toBe(true);
  expect(processing({ ...read, status: { ...read.status!, state: "full" } })).toBe(false);
});

it("keeps polling unattached uploads even when attached media is ready", () => {
  expect(processing({ ...read, files: [{ path: "pages/new.png", upload: true, unattached: true, type: "image/png", pending: ["low"] }] })).toBe(true);
});

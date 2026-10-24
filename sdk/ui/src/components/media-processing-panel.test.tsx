// @vitest-environment jsdom
import "../test/dom.js";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient, type ReadResult } from "../client/index.js";
import { MediaProcessingPanel } from "./media-processing-panel.js";

afterEach(cleanup);
const item = { kind: "comic", id: "01900000-0000-7000-8000-000000000001" };
it("shows server stage counts and only completed derivative images, with pending placeholders", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test" });
  const base: ReadResult = { access: "full", expires: 0, total: 3, offset: 0, limit: 1, files: [
    { path: "originals/1.png", type: "image/png", upload: true, size: 20, url: "https://private.test/original" },
    { path: "thumb/1.webp", from: "originals/1.png", type: "image/webp", w: 200, size: 10 },
    { path: "originals/2.png", type: "image/png", upload: true, size: 20, pending: ["thumb"] },
  ] };
  const status = { state: "processing", failure_count: 0, failures: [], uploads: { completed: 2, total: 2, failed: 0 }, archives: { completed: 0, total: 1, failed: 0, pages: { completed: 8, total: 25, failed: 0 } }, presets: [{ name: "thumb", completed: 1, total: 2, failed: 0 }] };
  vi.spyOn(client.media, "read").mockImplementation(async (_ref, options) => options?.status ? { ...base, files: [], status } : options?.paths ? { ...base, files: [{ ...base.files[1]!, url: "https://media.test/ready.webp" }] } : base);
  vi.spyOn(client.media, "watch").mockImplementation(async (_ref, _receive, signal) => new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true })));
  const view = render(<MediaProcessingPanel item={item} client={client} />);
  await waitFor(() => expect(screen.getByRole("img", { name: "1.png" })).toHaveAttribute("src", "https://media.test/ready.webp"));
  expect(view.container.querySelector('[data-stage="archive-pages"]')).toHaveTextContent("ZIP pages: 8/25");
  expect(view.container.querySelector('[data-stage="preset:thumb"]')).toHaveTextContent("thumb: 1/2");
  expect(view.container.querySelectorAll('[data-ckui="media-progress-preview"][data-state="pending"]')).toHaveLength(1);
  expect(view.container.querySelector('img[src="https://private.test/original"]')).toBeNull();
});

// @vitest-environment jsdom
import "../test/dom.js";
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import type { ReadResult } from "../client/generated/wire.js";
import { MediaReadinessNotice } from "./media-readiness-notice.js";

const item = { kind: "gallery", id: "01900000-0000-7000-8000-000000000001" };
const read = (status: ReadResult["status"]): ReadResult => ({ access: "full", expires: 0, total: 0, offset: 0, limit: 0, files: [], status });

it("shows ZIP, thumbnail and encoding progress without implying automatic publication", () => {
  render(<MediaReadinessNotice item={item} read={read({
    state: "processing", failure_count: 0, uploads: { completed: 5, total: 30, failed: 0 },
    archives: { completed: 0, total: 1, failed: 0, pages: { completed: 3, total: 30, failed: 0 } },
    presets: [{ name: "thumbnail", completed: 5, total: 30, failed: 0 }],
    encoding: { phase: "encoding", percent: 20, segments_done: 2, segments_total: 10 }, failures: [],
  })} />);
  expect(screen.getByText("ZIP extraction: 0/1")).toBeInTheDocument();
  expect(screen.getByText("ZIP pages: 3/30")).toBeInTheDocument();
  expect(screen.getByText("thumbnail: 5/30")).toBeInTheDocument();
  expect(screen.getByRole("progressbar", { name: "thumbnail" })).toHaveAttribute("aria-valuenow", expect.stringMatching(/^16/));
  expect(screen.getByText(/2.*10/)).toBeInTheDocument();
  expect(screen.getByText("Processing runs in the background. Publish separately when ready.")).toBeInTheDocument();
});

it("shows processing errors and disappears once the media is ready", () => {
  const status: NonNullable<ReadResult["status"]> = {
    state: "failed", failure_count: 1, uploads: { completed: 0, total: 1, failed: 1 }, archives: { completed: 0, total: 1, failed: 1 }, presets: [],
    failures: [{ path: "import/pages.zip", code: "invalid_archive", message: "No readable pages were found." }],
  };
  const { container, rerender } = render(<MediaReadinessNotice item={item} read={read(status)} />);
  expect(screen.getByRole("alert")).toHaveTextContent("No readable pages were found.");
  expect(screen.getByText("Fix or remove the failed files before publishing.")).toBeInTheDocument();
  rerender(<MediaReadinessNotice item={item} read={read({ ...status, state: "ready", failure_count: 0, failures: [] })} />);
  expect(container).toBeEmptyDOMElement();
});

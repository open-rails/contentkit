// @vitest-environment jsdom
import "../test/dom.js";
import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { UploadClient } from "../client.js";
import { ja } from "../locales/ja.js";
import { EncodeProgress, UploadUiProvider } from "../ui.js";
import type { EncodeProgress as Progress } from "../wire.gen.js";

beforeEach(() => vi.useFakeTimers({ now: 1_000_000 }));
afterEach(() => vi.useRealTimers());

const encoding = (over: Partial<Progress> = {}): Progress => ({
  phase: "encoding", segments_done: 5, segments_total: 27, percent: 18.5, speed: 2.4, eta: 40, at: 1, ...over,
});

it("shows the phase, segments and a local countdown between polls", () => {
  const { container, rerender } = render(<EncodeProgress progress={encoding()} />);
  const root = container.firstElementChild!;
  expect(root).toHaveClass("ckui");
  expect(root).toHaveAttribute("data-phase", "encoding");
  expect(screen.getByText("Encoding · segment 5 / 27 · ~40 s left")).toBeInTheDocument();
  const bar = screen.getByRole("progressbar", { name: "Processing video" });
  expect(bar).toHaveAttribute("aria-valuenow", "18.5");

  act(() => vi.advanceTimersByTime(25_000));
  expect(screen.getByText("Encoding · segment 5 / 27 · ~15 s left")).toBeInTheDocument();

  // A new report resets the countdown from its own ETA.
  rerender(<EncodeProgress progress={encoding({ segments_done: 9, eta: 150, at: 2, percent: 33 })} />);
  expect(screen.getByText("Encoding · segment 9 / 27 · ~3 min left")).toBeInTheDocument();
  rerender(<EncodeProgress progress={encoding({ phase: "uploading", eta: 3, at: 3, percent: 97 })} />);
  expect(screen.getByText("Saving · almost done")).toBeInTheDocument();
});

it("shows the queue position, an indeterminate bar before progress, and stalls", () => {
  const { rerender } = render(<EncodeProgress progress={{ phase: "queued", queue_position: 3, percent: 0, at: 1 }} />);
  expect(screen.getByText("Queued · #3 in line")).toBeInTheDocument();
  expect(screen.getByRole("progressbar")).not.toHaveAttribute("aria-valuenow");
  rerender(<EncodeProgress progress={encoding({ stalled: true, eta: undefined })} />);
  expect(screen.getByText("Processing paused, waiting for the server…")).toBeInTheDocument();
  rerender(<EncodeProgress />);
  expect(screen.getByText("Processing video")).toBeInTheDocument();
});

it("is translated", () => {
  const client = new UploadClient({ endpoint: "http://x/api" });
  render(
    <UploadUiProvider client={client} messages={ja}>
      <EncodeProgress progress={encoding({ eta: 3700 })} />
    </UploadUiProvider>,
  );
  expect(screen.getByText("エンコード中 · セグメント 5 / 27 · 残り約 1 時間 2 分")).toBeInTheDocument();
});

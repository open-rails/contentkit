// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, it, vi } from "vitest";
import type { ContentKitClient, CropSource, RefBody } from "../../src/client/index.js";
import { ko } from "../../src/locales/ko.js";
import { ContentKitUiProvider, VideoPosterPicker } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, fixture, harness, item, png, recorder, wait, type Recorder } from "./setup.js";

const h = harness();
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  owner = await h.user();
});

URL.createObjectURL ??= () => "blob:frame";
URL.revokeObjectURL ??= () => {};

function reducedMotion(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: on && q.includes("reduce"), addEventListener() {}, removeEventListener() {} }));
}

const round3 = (n: number) => Math.round(n * 1000) / 1000;

/** A video item with the 12 s clip as its source (probed by the worker) and its poster the frame at 3 s. */
async function video(): Promise<{ ref: RefBody; record: Recorder; c: ContentKitClient; duration: number }> {
  const { kind, id } = await item(h, "video", owner);
  const ref = { kind, id };
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  await c.media.put(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref, path: "source" });
  await c.media.commit(ref, [{ op: "frame", path: "poster", t: 3 }]);
  await c.media.waitFor(ref, "poster", wait);
  const source = (await c.media.read(ref, { editor: true })).files.find((f) => f.path === "source.mp4")!;
  record.frames.length = record.commits.length = record.calls.length = 0;
  return { ref, record, c, duration: source.dur! };
}

it("VideoPosterPicker: browse the strip, step frames, use the exact frame", async () => {
  reducedMotion(false);
  const { ref, record, c, duration } = await video();
  const onChange = vi.fn();
  const onOpenChange = vi.fn();
  const user = userEvent.setup();
  render(<VideoPosterPicker open onOpenChange={onOpenChange} item={ref} client={c} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  // Eight evenly spaced frames, one at a time, plus the exact frame of the current poster.
  await waitFor(() => expect(record.frames.filter((f) => f.endsWith("@160"))).toHaveLength(8), wait);
  await waitFor(() => expect(record.frames).toContain("3@960"), wait);
  expect(within(dialog).getByText("0:03.0")).toBeInTheDocument();

  // The fifth strip frame, then one frame on.
  await user.click(within(dialog).getAllByRole("button", { name: /^Jump to / })[4]!);
  await user.click(within(dialog).getByRole("button", { name: "Next frame" }));
  const t = round3((4.5 / 8) * duration) + 1 / 30;
  await waitFor(() => expect(Number(record.frames.at(-1)!.split("@")[0])).toBeCloseTo(t, 2), wait);
  expect(record.frames.at(-1)).toMatch(/@960$/);

  await user.click(within(dialog).getByRole("button", { name: "Use this frame" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled(), wait);
  const [op] = record.commits.at(-1)!;
  expect(op).toMatchObject({ op: "frame", path: "poster" });
  expect(op!.t).toBeCloseTo(t, 2);
  expect(op!.edit).toBeUndefined();
  expect(onChange.mock.calls[0]![0]).toMatchObject({ path: "poster.png", frame: { t: op!.t } });
  expect(onOpenChange).toHaveBeenCalledWith(false);
});

it("VideoPosterPicker: crop a frame in the video's pixels, wait for the render", async () => {
  reducedMotion(false);
  const { ref, record, c } = await video();
  const onChange = vi.fn();
  const user = userEvent.setup();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={ref} client={c} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await user.click(await within(dialog).findByRole("button", { name: "Crop…" }, wait));
  const crop = await screen.findByRole("dialog", { name: "Crop the cover" }, wait);
  // The crop draws the frame at the video's own width (640).
  expect(record.frames.some((f) => /^3@\d+$/.test(f) && f !== "3@960")).toBe(true);
  expect(within(crop).queryByRole("button", { name: "Rotate right" })).toBeNull();
  // The worker is held on the item: the picker waits for the render.
  await h.faults([{ item: ref.id, fault: "hold", from: "worker" }]);
  try {
    await user.click(within(crop).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(record.commits.length).toBe(1), wait);
    // The whole 16:9 frame: the crop at the video's aspect is the full frame.
    expect(record.commits.at(-1)![0]).toEqual({ op: "frame", path: "poster", t: 3 });
    await new Promise((r) => setTimeout(r, 500));
    expect(onChange).not.toHaveBeenCalled();
  } finally {
    await h.clearFaults(ref.id);
  }
  await waitFor(() => expect(onChange).toHaveBeenCalled(), wait);
  expect(record.calls.filter((p) => p === "/read").length).toBeGreaterThan(2);
});

it("VideoPosterPicker: upload an image, crop it, and return to automatic", async () => {
  reducedMotion(false);
  const { ref, record, c } = await video();
  // jsdom decodes no images: the decode seam reports the real file's size.
  const decode = vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:img", width: 800, height: 800, file }));
  const onChange = vi.fn();
  const user = userEvent.setup();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={ref} client={c} decode={decode} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("tab", { name: "Upload image" }));
  await user.upload(document.querySelector<HTMLInputElement>("[data-ckui=poster-drop] input")!, png("p.png", 4, 800, 800));
  const crop = await screen.findByRole("dialog", { name: "Crop the cover" });
  expect(within(crop).getByText(/800 px wide; 1920 px or more/)).toBeInTheDocument();
  await user.click(within(crop).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled(), wait);
  expect(record.calls).toContain("/presign");
  expect(record.commits.at(-1)![0]).toMatchObject({ op: "put", path: "poster.png", blob: expect.stringMatching(/^u-/) });

  onChange.mockClear();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={ref} client={c} onChange={onChange} />);
  const again = (await screen.findAllByRole("dialog")).at(-1)!;
  await user.click(await within(again).findByRole("button", { name: "Automatic" }, wait));
  await waitFor(() => expect(onChange).toHaveBeenCalled(), wait);
  expect(record.commits.at(-1)).toEqual([{ op: "frame", path: "poster", auto: true }]);
});

it("VideoPosterPicker: a video not probed yet says so; errors are localized", async () => {
  const { kind, id } = await item(h, "video", owner);
  const ref = { kind, id };
  const c = client(h, cfg, owner);
  // The worker is held on the item: its source is never placed or probed.
  await h.faults([{ item: ref.id, fault: "hold", from: "worker" }]);
  try {
    const up = await c.media.upload(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref, path: "source" });
    await c.media.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    render(
      <ContentKitProvider client={c}>
        <ContentKitUiProvider messages={ko}>
          <VideoPosterPicker open onOpenChange={() => {}} item={ref} />
        </ContentKitUiProvider>
      </ContentKitProvider>,
    );
    expect(await screen.findByText(ko.poster!.processing!, undefined, wait)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ko.poster!.useFrame! })).toBeNull();
  } finally {
    await h.clearFaults(ref.id);
  }
});

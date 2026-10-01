// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient } from "../../test/fake.js";
import type { CropSource } from "../image.js";
import { ko } from "../locales/ko.js";
import { UploadUiProvider, VideoPoster, VideoPosterPicker } from "../ui.js";

const item = { kind: "post", id: "0192f000-0000-7000-8000-000000000009" };

const source = { path: "source.mp4", type: "video/mp4", size: 100, dur: 12, w: 1920, h: 1080 };

function setup(video = source) {
  const s = new FakeServer();
  s.seed(item, [video, { path: "poster.png", type: "image/png", size: 10, w: 1920, h: 1080, frame: { t: 3 } }]);
  return { s, client: fakeClient(s) };
}

URL.createObjectURL ??= () => "blob:frame";
URL.revokeObjectURL ??= () => {};
globalThis.fetch = vi.fn(async () => new Response(null)) as typeof fetch;

const poster = { base: "https://cdn", namespace: "app", kind: "post", id: item.id, to: "poster-{w}.webp", widths: [480, 960], aspect: "16:9" };

function reducedMotion(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: on && q.includes("reduce"), addEventListener() {}, removeEventListener() {} }));
}

it("VideoPoster: the video's aspect from the preset, uncropped", () => {
  const tall = { ...poster, widths: [480], aspect: "480:853" };
  const { container } = render(<VideoPoster poster={tall} alt="tall" />);
  expect(container.firstElementChild).toHaveStyle({ aspectRatio: String(480 / 853) });
  expect(screen.getByRole("img", { name: "tall" })).toHaveClass("object-contain");
});

it("VideoPoster: srcset at the poster's aspect; a cover only, it never plays", () => {
  reducedMotion(false);
  const { container } = render(
    <VideoPoster poster={poster} alt="clip">
      <a href="#post">open</a>
    </VideoPoster>,
  );
  const root = container.firstElementChild!;
  expect(root).toHaveClass("ckui");
  expect(root).toHaveStyle({ aspectRatio: String(16 / 9) });
  expect(screen.getByRole("img", { name: "clip" })).toHaveAttribute("src", `https://cdn/v1/app/post/${item.id}/public/poster-480.webp`);
  fireEvent.pointerEnter(root);
  fireEvent.focus(screen.getByRole("link"));
  expect(container.querySelector("video")).toBeNull();
});

it("VideoPosterPicker: browse the strip, step frames, use the exact frame", async () => {
  reducedMotion(false);
  const { s, client } = setup();
  const onChange = vi.fn();
  const onOpenChange = vi.fn();
  const user = userEvent.setup();
  render(<VideoPosterPicker open onOpenChange={onOpenChange} item={item} client={client} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  // Eight evenly spaced frames, one at a time, plus the exact frame at a quarter in.
  await waitFor(() => expect(s.frames.filter((f) => f.endsWith("@160"))).toHaveLength(8));
  await waitFor(() => expect(s.frames).toContain("3@960"));
  expect(within(dialog).getByText("0:03.0")).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: "Jump to 0:06.8" }));
  await user.click(within(dialog).getByRole("button", { name: "Next frame" }));
  await waitFor(() => expect(s.frames.at(-1)).toMatch(/^6\.78\d*@960$/));

  await user.click(within(dialog).getByRole("button", { name: "Use this frame" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled());
  const [op] = s.commits.at(-1)!;
  expect(op).toMatchObject({ op: "frame", path: "poster" });
  expect(op!.t).toBeCloseTo(6.783, 3);
  expect(op!.edit).toBeUndefined();
  expect(onChange.mock.calls[0]![0]).toMatchObject({ path: "poster.png", frame: { t: op!.t } });
  expect(onOpenChange).toHaveBeenCalledWith(false);
});

it("VideoPosterPicker: crop a frame in the video's pixels, wait for the render", async () => {
  reducedMotion(false);
  const { s, client } = setup();
  s.pendingReads = 1;
  const onChange = vi.fn();
  const user = userEvent.setup();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={item} client={client} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await user.click(await within(dialog).findByRole("button", { name: "Crop…" }));
  const crop = await screen.findByRole("dialog", { name: "Crop the cover" });
  expect(s.frames).toContain("3@1280");
  expect(within(crop).queryByRole("button", { name: "Rotate right" })).toBeNull();
  await user.click(within(crop).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled(), { timeout: 4000 });
  // The whole 16:9 frame: the crop at the video's aspect is the full frame.
  expect(s.commits.at(-1)![0]).toEqual({ op: "frame", path: "poster", t: 3 });
  expect(s.calls.filter((c) => c === "/read").length).toBeGreaterThan(2);
});

it("VideoPosterPicker: upload an image, crop it, and return to automatic", async () => {
  reducedMotion(false);
  const { s, client } = setup();
  const decode = vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:img", width: 1600, height: 1600, file }));
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { container } = render(<VideoPosterPicker open onOpenChange={() => {}} item={item} client={client} decode={decode} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("tab", { name: "Upload image" }));
  await user.upload(document.querySelector<HTMLInputElement>("[data-ckui=poster-drop] input")!, new File([bytes(200, 4)], "p.png", { type: "image/png" }));
  const crop = await screen.findByRole("dialog", { name: "Crop the cover" });
  expect(within(crop).getByText(/1600 px wide; 1920 px or more/)).toBeInTheDocument();
  await user.click(within(crop).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled());
  expect(s.calls).toContain("/presign");
  expect(s.commits.at(-1)![0]).toMatchObject({ op: "put", path: "poster.png", blob: expect.stringMatching(/^u-/) });
  void container;

  onChange.mockClear();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={item} client={client} onChange={onChange} />);
  const again = (await screen.findAllByRole("dialog")).at(-1)!;
  await user.click(await within(again).findByRole("button", { name: "Automatic" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled());
  expect(s.commits.at(-1)).toEqual([{ op: "frame", path: "poster", auto: true }]);
});

it("VideoPosterPicker: a video not probed yet says so; errors are localized", async () => {
  const { client } = setup({ ...source, dur: undefined } as unknown as typeof source);
  render(
    <UploadUiProvider client={client} messages={ko}>
      <VideoPosterPicker open onOpenChange={() => {}} item={item} />
    </UploadUiProvider>,
  );
  expect(await screen.findByText(ko.poster!.processing!)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: ko.poster!.useFrame! })).toBeNull();
});

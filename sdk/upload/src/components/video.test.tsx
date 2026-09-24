// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes } from "../../test/fake.js";
import { UploadClient } from "../client.js";
import type { CropSource } from "../image.js";
import { ko } from "../locales/ko.js";
import { useHoverSection } from "../video-react.js";
import { HoverPreviewPicker, UploadUiProvider, VideoPoster, VideoPosterPicker } from "../ui.js";

const item = { kind: "post", id: "9" };

function setup() {
  const s = new FakeServer();
  const client = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
  return { s, client };
}

URL.createObjectURL ??= () => "blob:frame";
URL.revokeObjectURL ??= () => {};

const poster = {
  aspect: "16:9",
  pending: false,
  outputs: [
    { name: "poster_480", w: 480, h: 270, url: "https://cdn/poster_480.webp?v=1" },
    { name: "poster_960", w: 960, h: 540, url: "https://cdn/poster_960.webp?v=1" },
  ],
};
const preview = { mp4: "https://cdn/hover_preview_320.mp4?v=2", webp: "https://cdn/hover_preview_320.webp?v=2" };

function reducedMotion(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: on && q.includes("reduce"), addEventListener() {}, removeEventListener() {} }));
}

it("VideoPoster: native aspect from the poster, uncropped", () => {
  const tall = { aspect: "", pending: false, outputs: [{ name: "poster_480", w: 480, h: 853, url: "https://cdn/p.webp" }] };
  const { container } = render(<VideoPoster poster={tall} alt="tall" />);
  expect(container.firstElementChild).toHaveStyle({ aspectRatio: String(480 / 853) });
  expect(screen.getByRole("img", { name: "tall" })).toHaveClass("object-contain");
});

it("VideoPoster: srcset at the poster's aspect, plays the muted MP4 loop on hover and focus, WebP on MP4 failure", () => {
  reducedMotion(false);
  const { container } = render(
    <VideoPoster poster={poster} preview={preview} alt="clip">
      <a href="#post">open</a>
    </VideoPoster>,
  );
  const root = container.firstElementChild!;
  expect(root).toHaveClass("ckui");
  expect(root).toHaveStyle({ aspectRatio: String(16 / 9) });
  expect(screen.getByRole("img", { name: "clip" })).toHaveAttribute("src", "https://cdn/poster_480.webp?v=1");
  expect(container.querySelector("video")).toBeNull();

  fireEvent.pointerEnter(root);
  const video = container.querySelector("video")!;
  expect(video).toHaveAttribute("src", preview.mp4);
  expect(video.muted).toBe(true);
  expect(video.loop).toBe(true);
  expect(video).toHaveAttribute("playsinline");
  fireEvent.error(video);
  expect(container.querySelector("[data-ckui=hover-preview]")).toHaveAttribute("src", preview.webp);
  fireEvent.pointerLeave(root);
  expect(container.querySelector("[data-ckui=hover-preview]")).toBeNull();

  fireEvent.focus(screen.getByRole("link"));
  expect(container.querySelector("[data-ckui=hover-preview]")).not.toBeNull();
  fireEvent.blur(screen.getByRole("link"));
  expect(container.querySelector("[data-ckui=hover-preview]")).toBeNull();
});

it("VideoPoster: nothing plays with reduced motion; the host can drive playback", () => {
  reducedMotion(true);
  const { container, rerender } = render(<VideoPoster poster={poster} preview={preview} playing />);
  expect(container.querySelector("[data-ckui=hover-preview]")).toBeNull();
  reducedMotion(false);
  rerender(<VideoPoster key="2" poster={null} preview={preview} playing />);
  expect(container.querySelector("video")).toHaveAttribute("src", preview.mp4);
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
  expect(s.videoCalls.at(-1)).toMatchObject({ ref: item, source: "frame", file: "clip.mp4" });
  expect(s.videoCalls.at(-1).time).toBeCloseTo(6.783, 3);
  expect(s.videoCalls.at(-1).edit).toBeUndefined();
  expect(onChange.mock.calls[0]![0].poster.outputs).toHaveLength(3);
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
  expect(s.videoCalls.at(-1).edit ?? null).toBeNull();
  expect(s.calls.filter((c) => c === "/video-images").length).toBeGreaterThan(1);
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
  expect(s.videoCalls.at(-1)).toMatchObject({ source: "upload", sha256: expect.stringMatching(/^[0-9a-f]{64}$/) });
  void container;

  onChange.mockClear();
  render(<VideoPosterPicker open onOpenChange={() => {}} item={item} client={client} onChange={onChange} />);
  const again = (await screen.findAllByRole("dialog")).at(-1)!;
  await user.click(await within(again).findByRole("button", { name: "Automatic" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled());
  expect(s.videoCalls.at(-1)).toMatchObject({ source: "auto" });
});

it("VideoPosterPicker: a video still encoding says so; errors are localized", async () => {
  const { s, client } = setup();
  s.video = { ...s.video, video: { ...s.video.video!, encoded: false } };
  render(
    <UploadUiProvider client={client} messages={ko}>
      <VideoPosterPicker open onOpenChange={() => {}} item={item} />
    </UploadUiProvider>,
  );
  expect(await screen.findByText(ko.poster!.processing!)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: ko.poster!.useFrame! })).toBeNull();
});

it("useHoverSection keeps the section inside the video and 1–6 s", () => {
  const { client } = setup();
  const { result } = renderHook(() => useHoverSection(client, { ref: item, duration: 10 }));
  expect([result.current.start, result.current.length]).toEqual([2.5, 3]);
  act(() => result.current.setLength(20));
  expect(result.current.length).toBe(6);
  act(() => result.current.setStart(9));
  expect(result.current.start).toBe(4);
  act(() => result.current.setLength(0.2));
  expect(result.current.length).toBe(1);
  const short = renderHook(() => useHoverSection(client, { ref: item, duration: 2 }));
  expect([short.result.current.start, short.result.current.length]).toEqual([0, 2]);
});

it("HoverPreviewPicker: shows the section over the strip, saves it and plays the render", async () => {
  reducedMotion(false);
  const { s, client } = setup();
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { rerender } = render(<HoverPreviewPicker open onOpenChange={() => {}} item={item} client={client} onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Starts at 0:03.0")).toBeInTheDocument();
  expect(within(dialog).getByText("3.0 s")).toBeInTheDocument();
  expect(within(dialog).getByText("Approximate preview")).toBeInTheDocument();
  const section = dialog.querySelector<HTMLElement>("[data-ckui=section]")!;
  expect(section.style.left).toBe("25%");
  expect(section.style.width).toBe("25%");
  // Unchanged: nothing to save; the flip-book grabs frames of the section.
  expect(within(dialog).getByRole("button", { name: "Save preview" })).toBeDisabled();
  await waitFor(() => expect(s.frames.filter((f) => f.endsWith("@640")).length).toBeGreaterThan(0), { timeout: 3000 });

  const thumbs = dialog.querySelectorAll<HTMLInputElement>("input[type=range]");
  expect(thumbs).toHaveLength(2);
  fireEvent.keyDown(thumbs[1]!, { key: "ArrowRight" });
  await waitFor(() => expect(within(dialog).getByRole("button", { name: "Save preview" })).toBeEnabled());
  await user.click(within(dialog).getByRole("button", { name: "Save preview" }));
  await waitFor(() => expect(onChange).toHaveBeenCalled());
  expect(s.videoCalls.at(-1)).toMatchObject({ ref: item, file: "clip.mp4", start: 3 });
  expect(s.videoCalls.at(-1).duration).toBeGreaterThan(3);

  rerender(<HoverPreviewPicker open onOpenChange={() => {}} item={item} client={client} images={onChange.mock.calls[0]![0]} />);
  expect(await screen.findByText("Rendered preview")).toBeInTheDocument();
  expect(document.querySelector("[data-ckui=preview-stage] video")).toHaveAttribute("src", expect.stringContaining("hover_preview_640.mp4"));
});

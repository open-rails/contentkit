// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { GALLERY_VIEW_KEY } from "../gallery-react.js";
import { de } from "../locales/de.js";
import type { FileInfo, ReadResult } from "../wire.gen.js";
import { MediaGallery, UploadUiProvider } from "../ui.js";

const read = (access: string, files: FileInfo[]): ReadResult => ({ access, total: files.length, preview_limit: 0, offset: 0, limit: 50, expires: 0, files });
const img = (index: number, w = 800, h = 600): FileInfo => ({ index, name: `${index}.png`, type: "image/png", w, h, url: `https://m/${index}.webp` });
const vid = (index: number): FileInfo => ({ index, name: `${index}.mp4`, type: "video/mp4", w: 1080, h: 1920, duration: 42, hls: true });
const four = read("full", [img(0), img(1, 600, 900), vid(2), img(3)]);
const hlsBase = (f: FileInfo) => `/hls/${f.name}/`;

beforeEach(() => {
  localStorage.clear();
  window.matchMedia = vi.fn().mockImplementation(() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  HTMLMediaElement.prototype.pause = vi.fn();
  HTMLMediaElement.prototype.play = vi.fn(() => Promise.resolve());
});

const current = () => document.querySelector("[data-ckui=slide][data-current]")!;

it("carousel: arrows, keys and dots move one item at a time; neighbours only are rendered", async () => {
  const user = userEvent.setup();
  render(<MediaGallery read={four} hlsBase={hlsBase} />);
  const region = screen.getByRole("group", { name: "Media" });
  expect(region).toHaveAttribute("aria-roledescription", "carousel");
  expect(screen.getByText("1 / 4")).toBeInTheDocument();
  expect(current()).toHaveAccessibleName("1 of 4");
  // Slide 4 is two away: not rendered yet.
  expect(document.querySelectorAll("[data-ckui=slide]")[3]!.childElementCount).toBe(0);
  expect(screen.queryByRole("button", { name: "Previous" })).toBeNull();

  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText("2 / 4")).toBeInTheDocument();
  region.focus();
  await user.keyboard("{ArrowRight}");
  expect(current()).toHaveAccessibleName("3 of 4");
  expect(current().querySelector("[data-ckui=video-player]")).not.toBeNull();
  await user.keyboard("{ArrowLeft}");
  expect(screen.getByText("2 / 4")).toBeInTheDocument();
  await user.keyboard("{End}");
  expect(screen.getByText("4 / 4")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Next" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Go to item 1" }));
  expect(screen.getByRole("button", { name: "Go to item 1" })).toHaveAttribute("aria-current", "true");
  // Off-screen slides are inert and hidden from assistive tech.
  expect(document.querySelectorAll("[data-ckui=slide]")[1]).toHaveAttribute("aria-hidden", "true");
});

it("carousel: a horizontal swipe changes item, a vertical one scrolls, a short one springs back", () => {
  render(<MediaGallery read={four} hlsBase={hlsBase} />);
  const stage = current().parentElement!.parentElement!;
  Object.defineProperty(stage, "clientWidth", { value: 400 });
  const swipe = (x0: number, x1: number, y1 = 0, t = 400) => {
    fireEvent.pointerDown(stage, { pointerId: 1, clientX: x0, clientY: 100, button: 0, pointerType: "touch", timeStamp: 0 });
    fireEvent.pointerMove(stage, { pointerId: 1, clientX: (x0 + x1) / 2, clientY: 100 + y1 / 2, pointerType: "touch" });
    fireEvent.pointerMove(stage, { pointerId: 1, clientX: x1, clientY: 100 + y1, pointerType: "touch" });
    fireEvent.pointerUp(stage, { pointerId: 1, clientX: x1, clientY: 100 + y1, pointerType: "touch", timeStamp: t });
  };
  swipe(300, 100);
  expect(screen.getByText("2 / 4")).toBeInTheDocument();
  swipe(300, 280, 0, 400);
  expect(screen.getByText("2 / 4")).toBeInTheDocument();
  swipe(300, 290, 200);
  expect(screen.getByText("2 / 4")).toBeInTheDocument();
  swipe(100, 300);
  expect(screen.getByText("1 / 4")).toBeInTheDocument();
  // The first item resists a swipe past the start.
  swipe(100, 350);
  expect(screen.getByText("1 / 4")).toBeInTheDocument();
  // A touch swipe ends without a click: the next tap on an arrow still moves.
  swipe(300, 100);
  const next = screen.getByRole("button", { name: "Next" });
  fireEvent.pointerDown(next, { pointerId: 2, clientX: 390, clientY: 100, button: 0, pointerType: "touch" });
  fireEvent.pointerUp(next, { pointerId: 2, clientX: 390, clientY: 100, pointerType: "touch" });
  fireEvent.click(next);
  expect(screen.getByText("3 / 4")).toBeInTheDocument();
});

it("carousel: the arrow that reaches an end hands focus to the carousel, so arrow keys keep working", async () => {
  const user = userEvent.setup();
  render(<MediaGallery read={read("full", [img(0), img(1)])} />);
  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText("2 / 2")).toBeInTheDocument();
  expect(document.activeElement).toBe(screen.getByRole("group", { name: "Media" }));
  await user.keyboard("{ArrowLeft}");
  expect(screen.getByText("1 / 2")).toBeInTheDocument();
});

it("a multi-video gallery draws the poster on the video it was cut from, for any viewer", () => {
  const two = read("full", [vid(0), vid(1)]);
  const poster = { aspect: "16:9", outputs: [{ name: "poster_480", w: 480, h: 270, url: "https://m/poster.webp" }], pending: false, min_width: 480 };
  const preview = { mp4: [], webp: [], pending: false };
  const { rerender } = render(<MediaGallery read={two} hlsBase={hlsBase} videoImages={{ poster: { ...poster, file: "1.mp4" }, hover_preview: preview }} />);
  const posterIn = (i: number) => document.querySelectorAll("[data-ckui=slide]")[i]!.querySelector("img[src*='poster.webp']");
  expect(posterIn(0)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Next" }));
  expect(posterIn(1)).not.toBeNull();
  // An uploaded poster (no file) belongs to the first video.
  rerender(<MediaGallery read={two} hlsBase={hlsBase} videoImages={{ poster, hover_preview: preview }} />);
  fireEvent.click(screen.getByRole("button", { name: "Previous" }));
  expect(posterIn(0)).not.toBeNull();
});

it("the swiped-away video pauses; only the visible one is active", async () => {
  const user = userEvent.setup();
  render(<MediaGallery read={four} hlsBase={hlsBase} />);
  await user.click(screen.getByRole("button", { name: "Go to item 3" }));
  const video = current().querySelector("video")!;
  Object.defineProperty(video, "paused", { configurable: true, value: false });
  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(video.pause).toHaveBeenCalled();
});

it("view toggle: uncontrolled, remembered, and controlled", async () => {
  const user = userEvent.setup();
  const { unmount } = render(<MediaGallery read={four} hlsBase={hlsBase} />);
  expect(screen.getByRole("button", { name: "Carousel" })).toHaveAttribute("aria-pressed", "true");
  await user.click(screen.getByRole("button", { name: "Grid" }));
  expect(document.querySelector("[data-ckui=media-gallery]")).toHaveAttribute("data-view", "grid");
  expect(screen.getAllByRole("button", { name: /^Open item/ })).toHaveLength(4);
  expect(localStorage.getItem(GALLERY_VIEW_KEY)).toBe("grid");
  unmount();

  render(<MediaGallery read={four} hlsBase={hlsBase} />);
  expect(document.querySelector("[data-ckui=media-gallery]")).toHaveAttribute("data-view", "grid");
  // The video tile shows its duration.
  expect(screen.getByRole("button", { name: "Open item 3 of 4" })).toHaveTextContent("0:42");
  document.body.innerHTML = "";

  const onViewChange = vi.fn();
  render(<MediaGallery read={four} view="carousel" onViewChange={onViewChange} storageKey={null} />);
  await user.click(screen.getByRole("button", { name: "Grid" }));
  expect(onViewChange).toHaveBeenCalledWith("grid");
  expect(document.querySelector("[data-ckui=media-gallery]")).toHaveAttribute("data-view", "carousel");
});

it("storage failures fall back to the default view", () => {
  const get = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new Error("blocked");
  });
  render(<MediaGallery read={four} defaultView="grid" />);
  expect(document.querySelector("[data-ckui=media-gallery]")).toHaveAttribute("data-view", "grid");
  get.mockRestore();
});

it("grid → lightbox at the clicked item; arrows move, Esc closes and focus returns to the tile", async () => {
  const user = userEvent.setup();
  render(<MediaGallery read={four} hlsBase={hlsBase} defaultView="grid" storageKey={null} />);
  const tile = screen.getByRole("button", { name: "Open item 2 of 4" });
  await user.click(tile);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("2 / 4")).toBeInTheDocument();
  expect(dialog.closest(".ckui")).toHaveAttribute("data-ckui-theme", "dark");
  await user.keyboard("{ArrowRight}");
  expect(within(dialog).getByText("3 / 4")).toBeInTheDocument();
  // Tab never reaches the page behind (a browser wraps via the guards; see e2e).
  const page = tile.closest("[data-ckui=media-gallery]")!;
  for (let i = 0; i < 8; i++) {
    await user.tab();
    expect(page.contains(document.activeElement)).toBe(false);
  }
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(tile).toHaveFocus();
});

it("one item renders alone, without toggle or dots", () => {
  render(<MediaGallery read={read("full", [img(0, 1080, 1920)])} />);
  expect(screen.queryByRole("group", { name: "Layout" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Grid" })).toBeNull();
  expect(screen.getByRole("img", { name: "Image 1" })).toHaveAttribute("src", "https://m/0.webp");
  const stage = document.querySelector("[data-ckui=slide]")!.parentElement!.parentElement!;
  expect(stage).toHaveStyle({ aspectRatio: String(1080 / 1920) });
  expect(stage.style.maxHeight).toBe("");
});

it("locked items: blurred teaser with the host's unlock slot, no locked URLs", async () => {
  const user = userEvent.setup();
  const files: FileInfo[] = [
    { index: 0, name: "teaser", type: "image/webp", w: 960, h: 720, teaser: true, url: "https://m/blurred?t=x" },
    { index: 1, type: "image/png", locked: true },
    { index: 2, type: "video/mp4", locked: true },
    { index: 3, type: "image/png", locked: true },
  ];
  const renderLocked = vi.fn(({ count }: { count: number }) => <button type="button">Unlock {count}</button>);
  const { container } = render(<MediaGallery read={read("none", files)} renderLocked={renderLocked} />);
  expect(screen.getByText("3 more items are locked")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Unlock 3" })).toBeInTheDocument();
  expect(renderLocked).toHaveBeenCalledWith({ count: 3, videos: 1 });
  const srcs = [...container.querySelectorAll("img")].map((i) => i.getAttribute("src"));
  expect(srcs).toEqual(["https://m/blurred?t=x"]);
  await user.click(screen.getByRole("button", { name: "Unlock 3" }));

  document.body.innerHTML = "";
  render(<MediaGallery read={read("preview", [img(0), files[1]!])} defaultView="grid" storageKey={null} />);
  expect(screen.getByRole("button", { name: "Open item 2 of 2" })).toHaveTextContent("+1");
});

it("pending and failed videos, and translated labels", async () => {
  const files: FileInfo[] = [
    { index: 0, name: "a.mp4", type: "video/mp4", progress: { phase: "encoding", segments_done: 2, segments_total: 10, percent: 20, at: Date.now() } },
    { index: 1, name: "b.mp4", type: "video/mp4", failed: "aspect" },
  ];
  render(
    <UploadUiProvider messages={de}>
      <MediaGallery read={read("full", files)} hlsBase={hlsBase} />
    </UploadUiProvider>,
  );
  expect(screen.getByText(/Encoding|Kodier/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Raster" })).toBeInTheDocument();
  await act(async () => screen.getByRole("button", { name: "Weiter" }).click());
  expect(within(current() as HTMLElement).getByRole("alert")).toHaveTextContent("Dieses Video konnte nicht verarbeitet werden.");
});

it("a browser without HLS says so, with a code and Retry", async () => {
  const user = userEvent.setup();
  render(<MediaGallery read={read("full", [vid(0)])} hlsBase={hlsBase} />);
  await user.click(screen.getByRole("button", { name: "Play video" }));
  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent("This video can't be played in this browser.");
  expect(alert).toHaveTextContent("Code no-hls");
  expect(within(alert).getByRole("button", { name: "Try again" })).toBeInTheDocument();
});

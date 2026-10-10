// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { FakeServer, fakeClient } from "../../test/fake.js";
import { ContentKitUiProvider, ImageCropDialog, LazyMount, MediaGallery, MediaReadinessNotice, RenditionImg, SlotImage, SortableList, VideoPosterPicker } from "../index.js";
import { ContentKitProvider } from "../react/index.js";

afterEach(() => vi.unstubAllGlobals());

// jsdom lays nothing out: rows 40 px tall, one under the other, for dnd-kit's keyboard moves.
function layOut() {
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    const li = this.closest("li");
    const i = li?.parentElement ? [...li.parentElement.children].indexOf(li) : 0;
    return { x: 0, y: i * 40, top: i * 40, left: 0, bottom: i * 40 + 36, right: 300, width: 300, height: 36, toJSON: () => ({}) } as DOMRect;
  });
  return () => rect.mockRestore();
}

it("SortableList reorders from the keyboard and announces it in the provider's language", async () => {
  const restore = layOut();
  const moves: [number, number][] = [];
  function List() {
    const [items, setItems] = useState(["a", "b", "c"]);
    return (
      <SortableList
        items={items}
        id={(x) => x}
        name={(x) => `item ${x}`}
        onMove={(from, to) => {
          moves.push([from, to]);
          setItems((xs) => {
            const next = [...xs];
            next.splice(to, 0, ...next.splice(from, 1));
            return next;
          });
        }}
      >
        {(x, handle) => (
          <>
            {handle}
            <span>{x}</span>
          </>
        )}
      </SortableList>
    );
  }
  const { container } = render(
    <ContentKitUiProvider language="de">
      <List />
    </ContentKitUiProvider>,
  );
  const handle = await screen.findByRole("button", { name: "item a verschieben" });
  handle.focus();
  // dnd-kit listens for the moving keys a tick after it lifts.
  const tick = () => act(() => new Promise((r) => setTimeout(r, 20)));
  fireEvent.keyDown(handle, { key: " ", code: "Space" });
  await tick();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowDown", code: "ArrowDown" });
  await tick();
  fireEvent.keyDown(document.activeElement!, { key: " ", code: "Space" });
  await waitFor(() => expect(moves).toEqual([[0, 1]]));
  expect([...container.querySelectorAll("[data-ckui=sortable-list] > li span")].map((s) => s.textContent)).toEqual(["b", "a", "c"]);
  await waitFor(() => expect(document.body.textContent).toContain("item a an Position 2 von 3 abgelegt."));
  restore();
});

it("ImageCropDialog offers shapes, previews the result and resets to the starting shape", async () => {
  const onConfirm = vi.fn();
  const user = userEvent.setup();
  render(<ImageCropDialog open onOpenChange={() => {}} source={{ url: "blob:x", width: 4000, height: 3000 }} aspect="" aspects={["", "1:1", "16:9"]} initialEdit={{ rotate: 90 }} onConfirm={onConfirm} />);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByRole("img", { name: "Result preview" })).toBeInTheDocument();
  expect(within(dialog).getByText("3000 × 4000 px")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(onConfirm).toHaveBeenLastCalledWith({ rotate: 90 });

  await user.click(within(dialog).getByRole("button", { name: "16:9" }));
  expect(within(dialog).getByText("4000 × 2250 px")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(onConfirm).toHaveBeenLastCalledWith({ crop: { x: 0, y: 375, w: 4000, h: 2250 } });

  await user.click(within(dialog).getByRole("button", { name: "Reset" }));
  expect(within(dialog).getByRole("button", { name: "Original" })).toHaveAttribute("aria-pressed", "true");
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(onConfirm).toHaveBeenLastCalledWith({ rotate: 90 });
});

const outputs = [{ url: "https://m/v1/a/user/1/public/a-128-g.webp", w: 128, h: 128 }];

it("RenditionImg shows a fallback without renditions or after a failed load, a skeleton until loaded, and fits", () => {
  const { container, rerender } = render(<RenditionImg outputs={[]} fallback={<span>initials</span>} />);
  expect(screen.getByText("initials")).toBeInTheDocument();
  rerender(<RenditionImg outputs={outputs} fallback={<span>initials</span>} skeleton fit="contain" alt="a" />);
  const img = screen.getByRole("img", { name: "a" });
  expect(img).toHaveAttribute("data-loading");
  expect(img.style.objectFit).toBe("contain");
  fireEvent.load(img);
  expect(img).not.toHaveAttribute("data-loading");
  fireEvent.error(img);
  expect(screen.getByText("initials")).toBeInTheDocument();
  expect(container.querySelector("img")).toBeNull();

  rerender(<SlotImage image={{ preset: "avatar", renditions: outputs, aspect: "1:1" }} fallback={<span>none</span>} fit="contain" skeleton alt="s" />);
  expect(screen.getByRole("img", { name: "s" }).style.objectFit).toBe("contain");
  fireEvent.error(screen.getByRole("img", { name: "s" }));
  expect(screen.getByText("none")).toBeInTheDocument();
});

it("LazyMount mounts children near the viewport and, with leaveMargin, unmounts them far away", () => {
  const observers: { cb: IntersectionObserverCallback; margin?: string }[] = [];
  vi.stubGlobal(
    "IntersectionObserver",
    class {
      constructor(cb: IntersectionObserverCallback, o?: IntersectionObserverInit) {
        observers.push({ cb, margin: o?.rootMargin });
      }
      observe() {}
      disconnect() {}
    },
  );
  const fire = (margin: string, isIntersecting: boolean) =>
    act(() => observers.filter((o) => o.margin === margin).forEach((o) => o.cb([{ isIntersecting } as IntersectionObserverEntry], {} as IntersectionObserver)));
  render(
    <LazyMount margin="10px" leaveMargin="100px" placeholder={<i>later</i>}>
      <b>media</b>
    </LazyMount>,
  );
  expect(screen.getByText("later")).toBeInTheDocument();
  fire("10px", true);
  expect(screen.getByText("media")).toBeInTheDocument();
  fire("100px", false);
  expect(screen.getByText("later")).toBeInTheDocument();
});

const post = { kind: "post", id: "0192f000-0000-7000-8000-000000000021" };

it("MediaReadinessNotice shows processing with the video's progress, then the failed files, then nothing", async () => {
  const s = new FakeServer();
  const c = fakeClient(s);
  s.seed(post, [{ path: "videos/a.mp4", type: "video/mp4", size: 9, pending: ["hls"], progress: { phase: "queued", queue_position: 2, percent: 0, at: 1 } }]);
  const { container, rerender } = render(<MediaReadinessNotice client={c} item={post} />);
  expect(await screen.findByText("Processing media")).toBeInTheDocument();
  expect(screen.getByText("Queued · #2 in line")).toBeInTheDocument();
  s.seed(post, [
    { path: "videos/a.mp4", type: "video/mp4", size: 9, failed: { of: "hls", message: "x" } },
    { path: "images/b.png", type: "image/png", size: 3, failed: { of: "low", message: "x" } },
  ]);
  rerender(<MediaReadinessNotice client={c} item={post} prefix="" />);
  await act(async () => c.media.commit(post, [{ op: "meta", path: "videos/a.mp4", meta: {} }]).catch(() => {}));
  expect(await screen.findByText("2 files couldn't be processed: a.mp4, b.png")).toBeInTheDocument();
  s.seed(post, [{ path: "images/b.png", type: "image/png", size: 3 }]);
  await act(async () => c.media.commit(post, [{ op: "edit", path: "images/b.png" }]));
  await waitFor(() => expect(container.querySelector("[data-ckui=readiness]")).toBeNull());
});

it("MediaGallery item reads through the client and builds HLS bases on its media mount", async () => {
  const s = new FakeServer();
  const c = fakeClient(s);
  s.seed(post, [
    { path: "low-res/1.webp", type: "image/webp", size: 3, w: 400, h: 300, upload: false },
    { path: "low-res/2.webp", type: "image/webp", size: 3, w: 300, h: 400, upload: false },
  ]);
  render(
    <ContentKitProvider client={c}>
      <MediaGallery item={post} prefix="low-res/" storageKey={null} />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("1 / 2")).toBeInTheDocument();
  expect(s.reads.at(-1)!.get("prefix")).toBe("low-res/");
  expect(screen.getByRole("img", { name: "Image 1" })).toHaveAttribute("src", expect.stringContaining("fake://cdn/private/low-res/1.webp"));
  expect(c.media.hlsBase(post, "hls/")).toBe(`http://x/read/post/${post.id}/hls/hls/`);
});

it("VideoPosterPicker takes frames from another video and uploads the chosen one as the cover image", async () => {
  const s = new FakeServer();
  const c = fakeClient(s);
  const series = { kind: "series", id: "0192f000-0000-7000-8000-000000000031" };
  const video = { kind: "video", id: "0192f000-0000-7000-8000-000000000032" };
  s.seed(video, [{ path: "source.mp4", type: "video/mp4", size: 9, w: 1920, h: 1080, dur: 12 }]);
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(<VideoPosterPicker client={c} open onOpenChange={() => {}} item={series} path="cover" frames={{ item: video }} aspect="16:9" onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await waitFor(() => expect(s.frames.length).toBeGreaterThan(1));
  expect(within(dialog).queryByRole("button", { name: "Automatic" })).toBeNull();
  await user.click(await within(dialog).findByRole("button", { name: "Use this frame" }));
  await waitFor(() => expect(onChange).toHaveBeenCalledOnce());
  expect(s.frames).toContain("3@null");
  expect(s.presigns.at(-1)).toMatchObject({ path: "cover", type: "image/jpeg" });
  expect(s.commits.at(-1)).toEqual([{ op: "put", path: "cover.jpg", blob: expect.stringMatching(/^u-/) }]);
  expect(s.commits.flat().some((op) => op.op === "frame")).toBe(false);
});

it("MediaGallery renders nothing without a read or item", () => {
  const { container } = render(<MediaGallery read={null} />);
  expect(container.innerHTML).toBe("");
});

// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
// Client-free: the readiness notice, the gallery item read and the poster picker against the real
// ContentKit are in e2e/integration/media.test.tsx.
import { ContentKitUiProvider, ImageCropDialog, LazyMount, MediaGallery, RenditionImg, SlotImage, SortableList } from "../index.js";

afterEach(() => vi.unstubAllGlobals());

// jsdom lays nothing out: rows 40 px tall, one under the other, for dnd-kit's keyboard moves.
function layOut() {
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    // The drag overlay's row stands where the dragged row is.
    const li = this.closest("[data-overlay]") ? document.querySelector("li[data-dragging]") : this.closest("li");
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

it("MediaGallery renders nothing without a read or item", () => {
  const { container } = render(<MediaGallery read={null} />);
  expect(container.innerHTML).toBe("");
});

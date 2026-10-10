// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import type { Edit } from "../client/generated/wire.js";
import { ImageCropDialog, SlotImage } from "../index.js";

// Client-free: the slot flows against the real ContentKit are in e2e/integration/ui.test.tsx.
const cover = { preset: "cover", aspect: "3:1", renditions: [1500, 3000].map((w) => ({ url: `https://m/v1/app/channel/x/public/cover-${w}-generation.webp`, w, h: w / 3 })) };

it("SlotImage renders the preset's public files at its aspect, and a placeholder without one or when it fails", () => {
  const { container, rerender } = render(<SlotImage image={cover} alt="cover" />);
  const img = screen.getByRole("img", { name: "cover" });
  // jsdom lays nothing out: an unmeasured box gets the narrowest width.
  expect(img).toHaveAttribute("src", cover.renditions[0]!.url);
  expect(img).toHaveAttribute("height", "500");
  expect(container.firstElementChild).toHaveClass("ckui");
  expect(container.firstElementChild).toHaveStyle({ aspectRatio: "3" });
  fireEvent.error(img);
  expect(screen.queryByRole("img", { name: "cover" })).toBeNull();
  rerender(<SlotImage image={null} round />);
  expect(container.querySelector("[data-empty]")).not.toBeNull();
});

it("ImageCropDialog confirms the initial edit, zooms from the keyboard and rotates", async () => {
  const onConfirm = vi.fn();
  const user = userEvent.setup();
  render(
    <ImageCropDialog
      open
      onOpenChange={() => {}}
      source={{ url: "blob:x", width: 3000, height: 2000 }}
      aspect="3:1"
      initialEdit={{ crop: { x: 0, y: 500, w: 3000, h: 1000 } }}
      onConfirm={onConfirm}
    />,
  );
  const dialog = await screen.findByRole("dialog");
  const zoomOut = within(dialog).getByRole("button", { name: "Zoom out" });
  expect(zoomOut).toBeDisabled();
  fireEvent.keyDown(dialog.querySelector("[data-ckui=crop-stage]")!, { key: "+" });
  expect(zoomOut).toBeEnabled();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(onConfirm).toHaveBeenLastCalledWith({ crop: { x: 0, y: 500, w: 3000, h: 1000 } });

  await user.click(within(dialog).getByRole("button", { name: "Rotate right" }));
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  const edit = onConfirm.mock.lastCall![0];
  expect(edit.rotate).toBe(90);
  expect(edit.crop.h / edit.crop.w).toBeCloseTo(3, 1); // the output stays 3:1 once turned
});

const settle = () => new Promise((r) => setTimeout(r, 300));

it("ImageCropDialog reports an edit only when it changes, so onEditChange can set state", async () => {
  let renders = 0;
  const reported: (Edit | null)[] = [];
  const onConfirm = vi.fn();
  function Host() {
    const [, setEdit] = useState<Edit | null>(null);
    renders++;
    return (
      <ImageCropDialog
        open
        onOpenChange={() => {}}
        source={{ url: "blob:x", width: 400, height: 300 }}
        aspect="1:1"
        onEditChange={(e) => {
          reported.push(e);
          if (renders < 50) setEdit(e); // bounded so a loop fails instead of hanging
        }}
        onConfirm={onConfirm}
      />
    );
  }
  const user = userEvent.setup();
  render(<Host />);
  const dialog = await screen.findByRole("dialog");
  await settle();
  expect(renders).toBeLessThan(10);
  expect(reported).toEqual([{ crop: { x: 50, y: 0, w: 300, h: 300 } }]);
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(onConfirm).toHaveBeenLastCalledWith({ crop: { x: 50, y: 0, w: 300, h: 300 } });
});

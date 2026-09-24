// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes } from "../../test/fake.js";
import { UploadClient } from "../client.js";
import type { CropSource } from "../image.js";
import { ja } from "../locales/ja.js";
import { AvatarUpload, CoverUpload, ImageCropDialog, SlotImage, UploadUiProvider } from "../ui.js";

const item = { kind: "channel", id: "7" };

function setup() {
  const s = new FakeServer();
  const client = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
  return { s, client };
}

const decodeAs = (width: number, height: number) =>
  vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width, height, file }));
const png = (seed = 1) => new File([bytes(300, seed)], "a.png", { type: "image/png" });

it("SlotImage renders srcset and sizes, and a placeholder when empty", () => {
  const manifest = {
    aspect: 3,
    pending: false,
    outputs: [
      { name: "a", w: 1500, h: 500, url: "https://cdn/1500.webp" },
      { name: "b", w: 3000, h: 1000, url: "https://cdn/3000.webp" },
    ],
  };
  const { container, rerender } = render(<SlotImage manifest={manifest} sizes="720px" alt="cover" />);
  const img = screen.getByRole("img", { name: "cover" });
  expect(img).toHaveAttribute("srcset", "https://cdn/1500.webp 1500w, https://cdn/3000.webp 3000w");
  expect(img).toHaveAttribute("sizes", "720px");
  expect(container.firstElementChild).toHaveClass("ckui");
  expect(container.firstElementChild).toHaveStyle({ aspectRatio: "3" });
  rerender(<SlotImage manifest={null} round />);
  expect(screen.queryByRole("img", { name: "cover" })).toBeNull();
  expect(container.querySelector("[data-empty]")).not.toBeNull();
});

it("AvatarUpload: pick → crop dialog with a sharpness warning → save → shows the new image", async () => {
  const { s, client } = setup();
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={client} item={item} decode={decodeAs(400, 300)} onChange={onChange} />);
  await waitFor(() => expect(s.calls).toContain("/slot"));
  expect(screen.getByRole("img", { name: "No avatar" })).toBeInTheDocument();
  expect(screen.getByText("Square image, at least 512 px.")).toBeInTheDocument();

  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png());
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your avatar")).toBeInTheDocument();
  expect(within(dialog).getByText(/This crop is 300 px wide; 512 px or more/)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.slotCalls[0]).toMatchObject({ ref: item, slot: "avatar", edit: { crop: { x: 50, y: 0, w: 300, h: 300 } } });
  expect(onChange).toHaveBeenCalledOnce();
  expect(container.querySelector("img")?.getAttribute("srcset")).toContain("512w");
  expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
});

it("CoverUpload: edit crop re-renders from the original without uploading", async () => {
  const { s, client } = setup();
  const { manifest } = await client.uploadSlot(png(2), { ref: item, slot: "cover" });
  const puts = s.puts.length;
  const user = userEvent.setup();
  render(<CoverUpload client={client} item={item} manifest={manifest} decode={decodeAs(4000, 3000)} />);
  // The overlay and the mobile row both render; CSS shows one.
  await user.click(screen.getAllByRole("button", { name: "Edit crop" })[0]!);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your cover")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.slotCalls.at(-1)).toMatchObject({ slot: "cover", edit: { crop: { x: 0, w: 4000, h: 1333 } } });
  expect(s.slotCalls.at(-1)).not.toHaveProperty("sha256");
  expect(s.puts.length).toBe(puts);
});

it("maps UploadError codes to messages and keeps the dialog open to retry", async () => {
  const { s, client } = setup();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={client} item={item} manifest={null} decode={decodeAs(1024, 1024)} />);
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png(3));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).queryByText(/px or more/)).toBeNull();
  s.refuse = { status: 429, code: "rate_limited", error: "slow down", retry_after: 30 };
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  expect(await within(dialog).findByText("Too many uploads. Try again in 30 s.")).toBeInTheDocument();
  s.refuse = undefined;
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("shows an unreadable file inline, localized through the provider", async () => {
  const { client } = setup();
  const user = userEvent.setup();
  const bad = vi.fn(async () => {
    throw new Error("bad");
  });
  const { container } = render(
    <UploadUiProvider client={client} messages={ja} appearance={{ theme: "dark", variables: { primary: "red" } }}>
      <AvatarUpload item={item} manifest={null} decode={bad} />
    </UploadUiProvider>,
  );
  const root = container.querySelector(".ckui")!;
  expect(root).toHaveAttribute("data-ckui-theme", "dark");
  expect((root as HTMLElement).style.getPropertyValue("--ckui-primary")).toBe("red");
  expect(screen.getByText("アバター")).toBeInTheDocument();
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png(4));
  expect(await screen.findByRole("alert")).toHaveTextContent("このファイルは画像として開けません。");
});

it("ImageCropDialog confirms the initial edit, zooms from the keyboard and rotates", async () => {
  const onConfirm = vi.fn();
  const user = userEvent.setup();
  render(
    <ImageCropDialog
      open
      onOpenChange={() => {}}
      source={{ url: "blob:x", width: 3000, height: 2000 }}
      aspect={3}
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

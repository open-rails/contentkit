// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Profiler, useState } from "react";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient } from "../../test/fake.js";
import type { CropSource } from "../client/image.js";
import type { Edit, FileInfo, ReadResult } from "../client/generated/wire.js";
import { AvatarUpload, ContentKitUiProvider, CoverUpload, ImageCropDialog, SlotEditError, SlotEditMenu, SlotEditor, SlotImage, useSlotEditor } from "../index.js";
import { ContentKitProvider } from "../react/index.js";

const item = { kind: "channel", id: "0192f000-0000-7000-8000-000000000007" };
const preset = (name: string, aspect: string, widths: number[]) => ({ preset: name, aspect, renditions: widths.map((w) => ({ url: `https://m/v1/app/channel/${item.id}/public/${name}-${w}-generation.webp`, w, h: name === "avatar" ? w : w / 3 })) });
const avatar = preset("avatar", "1:1", [128, 256, 512]);
const cover = preset("cover", "3:1", [1500, 3000]);
const empty: ReadResult = { access: "full", expires: 0, total: 0, offset: 0, limit: 50, files: [] };
globalThis.fetch = vi.fn(async () => new Response(null)) as typeof fetch;

function setup() {
  const s = new FakeServer();
  s.publicImages.set(`${item.kind}/${item.id}`, [{ from: "avatar.png", ...avatar }, { from: "cover.png", ...cover }]);
  return { s, client: fakeClient(s) };
}

const decodeAs = (width: number, height: number) =>
  vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width, height, file }));
const png = (seed = 1) => new File([bytes(300, seed)], "a.png", { type: "image/png" });

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

it("AvatarUpload: pick → crop dialog with a sharpness warning → save → shows the new image", async () => {
  const { s, client } = setup();
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={client} item={item} image={avatar} decode={decodeAs(400, 300)} onChange={onChange} />);
  await waitFor(() => expect(s.calls).toContain("/read"));
  expect(screen.getByRole("img", { name: "No avatar" })).toBeInTheDocument();
  expect(screen.getByText("Square image, at least 512 px.")).toBeInTheDocument();

  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png());
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your avatar")).toBeInTheDocument();
  expect(within(dialog).getByText(/This crop is 300 px wide; 512 px or more/)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.commits[0]).toEqual([{ op: "put", path: "avatar.png", blob: expect.stringMatching(/^u-/), edit: { crop: { x: 50, y: 0, w: 300, h: 300 } } }]);
  expect(onChange).toHaveBeenCalledOnce();
  expect(container.querySelector("img")?.getAttribute("data-rendition")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
});

it("AvatarUpload removes the avatar; CoverUpload offers Remove only when removable", async () => {
  const { s, client } = setup();
  await client.media.put(png(3), { ref: item, path: "avatar" });
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { unmount } = render(<AvatarUpload client={client} item={item} image={avatar} onChange={onChange} />);
  await user.click(await screen.findByRole("button", { name: "Remove" }));
  await waitFor(() => expect(screen.getByRole("img", { name: "No avatar" })).toBeInTheDocument());
  expect(s.commits.at(-1)).toEqual([{ op: "remove", path: "avatar.png" }]);
  expect(onChange).toHaveBeenLastCalledWith(null);
  expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
  unmount();

  await client.media.put(png(4), { ref: item, path: "cover" });
  const { rerender } = render(<CoverUpload client={client} item={item} image={cover} />);
  await screen.findAllByRole("button", { name: "Change" });
  expect(screen.queryAllByRole("button", { name: "Remove" })).toHaveLength(0);
  rerender(<CoverUpload client={client} item={item} image={cover} removable />);
  expect((await screen.findAllByRole("button", { name: "Remove" })).length).toBeGreaterThan(0);
});

it("CoverUpload: edit crop re-renders from the upload without uploading", async () => {
  const { s, client } = setup();
  await client.media.put(png(2), { ref: item, path: "cover" });
  const puts = s.puts.length;
  const user = userEvent.setup();
  render(<CoverUpload client={client} item={item} image={cover} decode={decodeAs(4000, 3000)} />);
  // The overlay and the mobile row both render; CSS shows one.
  await user.click((await screen.findAllByRole("button", { name: "Edit crop" }))[0]!);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your cover")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.commits.at(-1)![0]).toMatchObject({ op: "edit", path: "cover.png", edit: { crop: { x: 0, w: 4000, h: 1333 } } });
  expect(s.puts.length).toBe(puts);
});

it("maps ContentKitError codes to messages and keeps the dialog open to retry", async () => {
  const { s, client } = setup();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={client} item={item} read={empty} decode={decodeAs(1024, 1024)} />);
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

it("shows an unreadable file inline, in the provider's language, and reports it to the provider", async () => {
  const { client } = setup();
  const user = userEvent.setup();
  const bad = vi.fn(async () => {
    throw new Error("bad");
  });
  const onError = vi.fn();
  const { container } = render(
    <ContentKitProvider client={client} onError={onError}>
      <ContentKitUiProvider language="ja-JP" appearance={{ theme: "dark", variables: { primary: "red" } }}>
        <AvatarUpload item={item} read={empty} decode={bad} />
      </ContentKitUiProvider>
    </ContentKitProvider>,
  );
  const root = container.querySelector(".ckui")!;
  expect(root).toHaveAttribute("data-ckui-theme", "dark");
  expect((root as HTMLElement).style.getPropertyValue("--ckui-primary")).toBe("red");
  expect(await screen.findByText("アバター")).toBeInTheDocument();
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png(4));
  expect(await screen.findByRole("alert")).toHaveTextContent("このファイルは画像として開けません。");
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "decode" }), { operation: "slot.decode" });
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

it("SlotEditor composes a host-styled overlay trigger: pick, then a Change / Edit crop menu", async () => {
  const { s, client } = setup();
  const user = userEvent.setup();
  const onChange = vi.fn();
  function Header() {
    const { has } = useSlotEditor();
    return <SlotImage image={has ? cover : null} alt="cover" />;
  }
  // The host owns the editor read (e.g. from its channel API) and keeps each save.
  function Host() {
    const [read, setRead] = useState<ReadResult>(empty);
    return (
      <div data-testid="overlay" className="host-overlay">
        <SlotEditor
          client={client}
          item={item}
          path="cover"
          image={cover}
          read={read}
          aspect="3:1"
          decode={decodeAs(3000, 1500)}
          onChange={(f: FileInfo | null) => {
            onChange(f);
            setRead({ ...read, files: f ? [f] : [] });
          }}
        >
          <Header />
          <SlotEditMenu label="Change cover" iconOnly render={<button className="host-btn" />} />
          <SlotEditError />
        </SlotEditor>
      </div>
    );
  }
  const { container } = render(<Host />);
  const overlay = screen.getByTestId("overlay");
  // No wrapper: the host's trigger sits directly in the host layout, unstyled by the kit.
  const trigger = within(overlay).getByRole("button", { name: "Change cover" });
  expect(trigger.parentElement).toBe(overlay);
  expect(trigger.className).toBe("host-btn");
  expect(trigger.closest(".ckui")).toBeNull();

  await user.click(trigger);
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png(5));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your cover")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(onChange).toHaveBeenCalledOnce();
  await waitFor(() => expect(screen.getByRole("img", { name: "cover" })).toHaveAttribute("src", cover.renditions[0]!.url));

  // Now the path has an upload: the same trigger opens a menu.
  const puts = s.puts.length;
  await user.click(within(overlay).getByRole("button", { name: "Change cover" }));
  const menu = await screen.findByRole("menu");
  expect(menu.closest(".ckui")).not.toBeNull();
  expect(within(menu).getByRole("menuitem", { name: "Change" })).toBeInTheDocument();
  await user.click(within(menu).getByRole("menuitem", { name: "Edit crop" }));
  const again = await screen.findByRole("dialog");
  await user.click(within(again).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.puts.length).toBe(puts);
  expect(s.commits.at(-1)![0]!.op).toBe("edit");
});

it("SlotEditMenu defaults to the kit's scoped button and SlotEditError shows unreadable files", async () => {
  const { client } = setup();
  const user = userEvent.setup();
  const bad = vi.fn(async () => {
    throw new Error("bad");
  });
  const { container } = render(
    <SlotEditor client={client} item={item} path="avatar" read={empty} decode={bad}>
      <SlotEditMenu />
      <SlotEditError />
    </SlotEditor>,
  );
  const trigger = screen.getByRole("button", { name: "Upload" });
  expect(trigger).toHaveClass("ckui");
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png(6));
  expect(await screen.findByRole("alert")).toHaveTextContent("This file can't be opened as an image.");
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

it("CoverUpload's crop dialog settles instead of re-rendering forever", async () => {
  const { client } = setup();
  await client.media.put(png(5), { ref: item, path: "cover" });
  let commits = 0;
  const user = userEvent.setup();
  render(
    <Profiler id="cover" onRender={() => commits++}>
      <CoverUpload client={client} item={item} image={cover} decode={decodeAs(4000, 3000)} />
    </Profiler>,
  );
  await user.click((await screen.findAllByRole("button", { name: "Edit crop" }))[0]!);
  await screen.findByRole("dialog");
  await settle();
  const settled = commits;
  await settle();
  expect(commits).toBe(settled);
});

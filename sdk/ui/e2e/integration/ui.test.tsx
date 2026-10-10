// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Profiler, useState } from "react";
import { beforeAll, expect, it, vi } from "vitest";
import type { CropSource, FileInfo, ReadResult, RefBody } from "../../src/client/index.js";
import { AvatarUpload, ContentKitUiProvider, CoverUpload, SlotEditError, SlotEditMenu, SlotEditor, SlotImage, useSlotEditor } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, harness, item, png, recorder, wait } from "./setup.js";

const h = harness();
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  owner = await h.user();
});

/** A channel (avatar 1:1, cover 3:1 public presets) its owner edits. */
async function channel(): Promise<RefBody> {
  const { kind, id } = await item(h, "channel", owner);
  return { kind, id };
}

function setup() {
  const record = recorder();
  return { record, c: client(h, cfg, owner, { record }) };
}

// The presets the components show; their renditions come from the item's reads.
const avatar = { preset: "avatar", aspect: "1:1", renditions: [] };
const cover = { preset: "cover", aspect: "3:1", renditions: [] };
const empty: ReadResult = { access: "full", expires: 0, total: 0, offset: 0, limit: 50, files: [] };

// jsdom decodes no images: the components' decode seam reports the real file's size.
const decodeAs = (width: number, height: number) =>
  vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width, height, file }));

it("AvatarUpload: pick → crop dialog with a sharpness warning → save → shows the new image", async () => {
  const ref = await channel();
  const { record, c } = setup();
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={c} item={ref} image={avatar} decode={decodeAs(400, 300)} onChange={onChange} />);
  await waitFor(() => expect(record.calls).toContain("/read"));
  expect(await screen.findByRole("img", { name: "No avatar" })).toBeInTheDocument();
  expect(screen.getByText("Square image, at least 512 px.")).toBeInTheDocument();

  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("a.png", 1, 400, 300));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your avatar")).toBeInTheDocument();
  expect(within(dialog).getByText(/This crop is 300 px wide; 512 px or more/)).toBeInTheDocument();

  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
  expect(record.commits[0]).toEqual([{ op: "put", path: "avatar.png", blob: expect.stringMatching(/^u-/), edit: { crop: { x: 50, y: 0, w: 300, h: 300 } } }]);
  expect(onChange).toHaveBeenCalledOnce();
  // The worker published the avatar's renditions.
  await waitFor(() => expect(container.querySelector("img")?.getAttribute("data-rendition")).toBeTruthy(), wait);
  expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
});

it("AvatarUpload removes the avatar; CoverUpload offers Remove only when removable", async () => {
  const ref = await channel();
  const { record, c } = setup();
  await c.media.put(png("a.png", 3, 300, 300), { ref, path: "avatar" });
  const onChange = vi.fn();
  const user = userEvent.setup();
  const { unmount } = render(<AvatarUpload client={c} item={ref} image={avatar} onChange={onChange} />);
  await user.click(await screen.findByRole("button", { name: "Remove" }, wait));
  await waitFor(() => expect(screen.getByRole("img", { name: "No avatar" })).toBeInTheDocument(), wait);
  expect(record.commits.at(-1)).toEqual([{ op: "remove", path: "avatar.png" }]);
  expect(onChange).toHaveBeenLastCalledWith(null);
  expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
  unmount();

  await c.media.put(png("c.png", 4, 600, 200), { ref, path: "cover" });
  const { rerender } = render(<CoverUpload client={c} item={ref} image={cover} />);
  await screen.findAllByRole("button", { name: "Change" }, wait);
  expect(screen.queryAllByRole("button", { name: "Remove" })).toHaveLength(0);
  rerender(<CoverUpload client={c} item={ref} image={cover} removable />);
  expect((await screen.findAllByRole("button", { name: "Remove" })).length).toBeGreaterThan(0);
});

it("CoverUpload: edit crop re-renders from the upload without uploading", async () => {
  const ref = await channel();
  const { record, c } = setup();
  await c.media.put(png("c.png", 2, 600, 450), { ref, path: "cover" });
  const puts = record.puts.length;
  const user = userEvent.setup();
  render(<CoverUpload client={c} item={ref} image={cover} />);
  // The overlay and the mobile row both render; CSS shows one.
  await user.click((await screen.findAllByRole("button", { name: "Edit crop" }, wait))[0]!);
  // Re-cropping draws the upload's editor view, which the worker renders.
  const dialog = await screen.findByRole("dialog", undefined, wait);
  expect(within(dialog).getByText("Crop your cover")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
  expect(record.commits.at(-1)![0]).toMatchObject({ op: "edit", path: "cover.png", edit: { crop: { x: 0, w: 600, h: 200 } } });
  expect(record.puts.length).toBe(puts);
});

it("SlotEditor crops at the path's public preset before the item has an image", async () => {
  const ref = await channel();
  const { record, c } = setup();
  const user = userEvent.setup();
  function Aspect() {
    return <output aria-label="aspect">{useSlotEditor().crop.aspect}</output>;
  }
  const { container } = render(
    <SlotEditor client={c} item={ref} path="cover" read={empty} decode={decodeAs(600, 450)}>
      <Aspect />
    </SlotEditor>,
  );
  // Neither an image nor an aspect from the host: the cover preset's 3:1, not a square.
  await waitFor(() => expect(screen.getByRole("status", { name: "aspect" })).toHaveTextContent("3:1"), wait);
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("c.png", 6, 600, 450));
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
  expect(record.commits.at(-1)).toEqual([expect.objectContaining({ op: "put", path: "cover.png", edit: { crop: { x: 0, y: 125, w: 600, h: 200 } } })]);
});

it("maps ContentKitError codes to messages and keeps the dialog open to retry", async () => {
  const ref = await channel();
  const { c } = setup();
  const user = userEvent.setup();
  const { container } = render(<AvatarUpload client={c} item={ref} read={empty} decode={decodeAs(512, 512)} />);
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("a.png", 3, 512, 512));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).queryByText(/px or more/)).toBeNull();
  // The upload API refuses this item's next upload.
  await h.faults([{ item: ref.id, fault: "api", path: "/media/upload/presign", method: "POST", status: 429, code: "rate_limited", error: "slow down", retry_after: 30 }]);
  try {
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await within(dialog).findByText("Too many uploads. Try again in 30 s.", undefined, wait)).toBeInTheDocument();
  } finally {
    await h.clearFaults(ref.id);
  }
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
});

it("shows an unreadable file inline, in the provider's language, and reports it to the provider", async () => {
  const ref = await channel();
  const { c } = setup();
  const user = userEvent.setup();
  const bad = vi.fn(async () => {
    throw new Error("bad");
  });
  const onError = vi.fn();
  const { container } = render(
    <ContentKitProvider client={c} onError={onError}>
      <ContentKitUiProvider language="ja-JP" appearance={{ theme: "dark", variables: { primary: "red" } }}>
        <AvatarUpload item={ref} read={empty} decode={bad} />
      </ContentKitUiProvider>
    </ContentKitProvider>,
  );
  const root = container.querySelector(".ckui")!;
  expect(root).toHaveAttribute("data-ckui-theme", "dark");
  expect((root as HTMLElement).style.getPropertyValue("--ckui-primary")).toBe("red");
  expect(await screen.findByText("アバター")).toBeInTheDocument();
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("a.png", 4));
  expect(await screen.findByRole("alert")).toHaveTextContent("このファイルは画像として開けません。");
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "decode" }), { operation: "slot.decode" });
});

it("SlotEditor composes a host-styled overlay trigger: pick, then a Change / Edit crop menu", async () => {
  const ref = await channel();
  const { record, c } = setup();
  const user = userEvent.setup();
  const onChange = vi.fn();
  // The host draws its own header image.
  const shown = { preset: "cover", aspect: "3:1", renditions: [{ url: `${cfg.media}/v1/${cfg.namespace}/channel/${ref.id}/public/cover-1500-host.webp`, w: 1500, h: 500 }] };
  function Header() {
    const { has } = useSlotEditor();
    return <SlotImage image={has ? shown : null} alt="cover" />;
  }
  // The host owns the editor read (e.g. from its channel API) and keeps each save.
  function Host() {
    const [read, setRead] = useState<ReadResult>(empty);
    return (
      <div data-testid="overlay" className="host-overlay">
        <SlotEditor
          client={c}
          item={ref}
          path="cover"
          image={cover}
          read={read}
          aspect="3:1"
          decode={decodeAs(600, 300)}
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
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("c.png", 5, 600, 300));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Crop your cover")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
  expect(onChange).toHaveBeenCalledOnce();
  await waitFor(() => expect(screen.getByRole("img", { name: "cover" })).toHaveAttribute("src", shown.renditions[0]!.url));

  // Now the path has an upload: the same trigger opens a menu.
  const puts = record.puts.length;
  await user.click(within(overlay).getByRole("button", { name: "Change cover" }));
  const menu = await screen.findByRole("menu");
  expect(menu.closest(".ckui")).not.toBeNull();
  expect(within(menu).getByRole("menuitem", { name: "Change" })).toBeInTheDocument();
  await user.click(within(menu).getByRole("menuitem", { name: "Edit crop" }));
  const again = await screen.findByRole("dialog", undefined, wait);
  await user.click(within(again).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
  expect(record.puts.length).toBe(puts);
  expect(record.commits.at(-1)![0]!.op).toBe("edit");
});

it("SlotEditMenu defaults to the kit's scoped button and SlotEditError shows unreadable files", async () => {
  const ref = await channel();
  const { c } = setup();
  const user = userEvent.setup();
  const bad = vi.fn(async () => {
    throw new Error("bad");
  });
  const { container } = render(
    <SlotEditor client={c} item={ref} path="avatar" read={empty} decode={bad}>
      <SlotEditMenu />
      <SlotEditError />
    </SlotEditor>,
  );
  const trigger = screen.getByRole("button", { name: "Upload" });
  expect(trigger).toHaveClass("ckui");
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, png("a.png", 6));
  expect(await screen.findByRole("alert")).toHaveTextContent("This file can't be opened as an image.");
});

const settle = () => new Promise((r) => setTimeout(r, 300));

it("CoverUpload's crop dialog settles instead of re-rendering forever", async () => {
  const ref = await channel();
  const { c } = setup();
  await c.media.put(png("c.png", 7, 600, 450), { ref, path: "cover" });
  let commits = 0;
  const user = userEvent.setup();
  render(
    <Profiler id="cover" onRender={() => commits++}>
      <CoverUpload client={c} item={ref} image={cover} />
    </Profiler>,
  );
  await user.click((await screen.findAllByRole("button", { name: "Edit crop" }, wait))[0]!);
  await screen.findByRole("dialog", undefined, wait);
  await settle();
  const settled = commits;
  await settle();
  expect(commits).toBe(settled);
});

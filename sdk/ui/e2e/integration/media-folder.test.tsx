// @vitest-environment jsdom
import "../../src/test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef } from "react";
import { beforeAll, expect, it, vi } from "vitest";
import type { RefBody } from "../../src/client/index.js";
import { MediaFolderEditor, type MediaFolderEditorHandle } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import { bytes } from "../support/bytes.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, fixture, harness, item, png, recorder, wait, type Recorder, NodeFile } from "./setup.js";

const h = harness();
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  owner = await h.user();
});

/** An item of kind owned by owner: note (originals/{name}, PNG, at most 2) or album (images and videos). */
async function folder(kind: "note" | "album"): Promise<RefBody> {
  const { kind: k, id } = await item(h, kind, owner);
  return { kind: k, id };
}

/** Bytes no decoder reads, declared as type: the worker refuses them. */
const garbage = (name: string, type: string, seed = 1) => new NodeFile([bytes(4096, seed)], name, { type }) as unknown as File;

const ops = (r: Recorder) => r.commits.map((c) => c.map((op) => `${op.op} ${op.path}${op.to ? ` → ${op.to}` : ""}`));

function setup() {
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  const onError = vi.fn();
  const user = userEvent.setup({ applyAccept: false });
  return { record, c, onError, user };
}

it("screens files against the kind's rules, uploads the rest and adds them in queue order", async () => {
  const ref = await folder("note");
  const { record, c, onError, user } = setup();
  const { container } = render(
    <ContentKitProvider client={c} onError={onError}>
      <MediaFolderEditor item={ref} />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("Drop files here, or click to choose")).toBeInTheDocument();
  // The rules come from the editor read: originals/{name}, PNG up to 1 MB, at most 2.
  expect(await screen.findByText("Images up to 1 MB, at most 2")).toBeInTheDocument();
  const input = container.querySelector<HTMLInputElement>("[data-ckui=folder-drop] input[type=file]")!;
  expect(input.accept).toContain("image/png");

  await user.upload(input, [png("b.png", 1), png("a.png", 2), garbage("c.gif", "image/gif")]);
  const refused = await screen.findByRole("alert");
  expect(refused).toHaveTextContent("c.gif: This file type isn't supported here.");
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "type_not_allowed" }), { operation: "upload", file: "c.gif" });

  await waitFor(() => expect(screen.getAllByText("Uploaded")).toHaveLength(2), wait);
  expect(record.presigns.map((p) => p.path)).toEqual(["originals/b.png", "originals/a.png"]);
  await user.click(screen.getByRole("button", { name: "Add 2" }));
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2), wait);
  expect(ops(record)).toEqual([["put originals/b.png", "put originals/a.png"]]);
  expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(0);
  expect([...container.querySelectorAll("[data-ckui=upload-name]")].map((n) => n.textContent)).toEqual(["b.png", "a.png"]);

  // The cap counts files already there: the item is full.
  await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-add] input")!, [png("d.png", 3)]);
  expect(await screen.findByText(/d\.png/)).toBeInTheDocument();
  expect(screen.getByRole("alert")).toHaveTextContent("This item holds at most 2 of these files. Remove some first.");
});

it("numbers a name already taken", async () => {
  const ref = await folder("album");
  const { record, c, user } = setup();
  const { container } = render(<MediaFolderEditor client={c} item={ref} />);
  await screen.findByText("Drop files here, or click to choose");
  // The album's rules: images and videos, and the video shapes its HLS ladder takes.
  expect(await screen.findByText("Images up to 25 MB, at most 6 · Videos up to 1 GB, at most 2 · video shapes from 1:2.4 to 2.4:1")).toBeInTheDocument();
  await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-drop] input[type=file]")!, [png("v.png", 5), png("v.png", 6)]);
  await waitFor(() => expect(record.presigns.map((p) => p.path).sort()).toEqual(["images/v-2.png", "images/v.png"]), wait);
});

it("renames, crops, replaces and removes uploads, and shows each one's thumbnail and state", async () => {
  const ref = await folder("album");
  const { record, c, user } = setup();
  // Two photos (b already turned), processed; then a clip whose processing the worker is held on.
  await c.media.put(png("a.png", 7, 400, 300), { ref, path: "images/a.png" });
  await c.media.put(png("b.png", 8, 400, 300), { ref, path: "images/b.png", edit: { rotate: 90 } });
  await c.media.waitFor(ref, "images/a.png", wait);
  await c.media.waitFor(ref, "images/b.png", wait);
  await h.faults([{ item: ref.id, fault: "hold", from: "worker" }]);
  const clip = await c.media.upload(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref, path: "videos/v.mp4" });
  await c.media.commit(ref, [{ op: "put", path: clip.path, blob: clip.blob }]);
  record.commits.length = 0;

  try {
    const { container } = render(<MediaFolderEditor client={c} item={ref} confirmRemove={false} />);
    await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(3), wait);
    const row = (path: string) => container.querySelector<HTMLElement>(`[data-ckui=upload-row][data-path="${path}"]`)!;
    // A processed photo shows its derived file, signed, from media-gateway.
    await waitFor(() => expect(row("images/a.png").querySelector("img")).toHaveAttribute("src", expect.stringContaining(`${cfg.media}/v1/${cfg.namespace}/album/${ref.id}/private/`)), wait);
    expect(within(row("images/b.png")).getByText("Edited")).toBeInTheDocument();
    expect(row("videos/v.mp4")).toHaveTextContent(/Processing|Queued|Encoding/);
    expect(container.querySelectorAll("[data-ckui=folder-group] h4")[0]).toHaveTextContent("Images · 2 / 6");
    // The worker renders the editor views the crop dialog draws.
    await h.clearFaults(ref.id);

    await user.click(within(row("images/a.png")).getByRole("button", { name: "Rename" }));
    const name = screen.getByRole("textbox", { name: "New name for a.png" });
    await user.clear(name);
    await user.type(name, "first{Enter}");
    await waitFor(() => expect(ops(record).at(-1)).toEqual(["rename images/a.png → images/first"]), wait);
    await waitFor(() => expect(row("images/first.png")).not.toBeNull(), wait);

    // The crop dialog draws the upload's editor view, rendered by the worker.
    await user.click(within(row("images/b.png")).getByRole("button", { name: "Crop and rotate" }));
    const dialog = await screen.findByRole("dialog", undefined, wait);
    expect(await within(dialog).findByRole("button", { name: "Original" }, wait)).toHaveAttribute("aria-pressed", "true");
    await user.click(within(dialog).getByRole("button", { name: "1:1" }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), wait);
    expect(record.commits.at(-1)).toEqual([{ op: "edit", path: "images/b.png", edit: { crop: { x: 50, y: 0, w: 300, h: 300 } } }]);

    const puts = record.puts.length;
    await user.click(within(row("images/b.png")).getByRole("button", { name: "Replace" }));
    fireEvent.change(container.querySelector<HTMLInputElement>("input[aria-hidden]")!, { target: { files: [fixture("media/img/1.jpg", "image/jpeg", "new.jpg")] } });
    await waitFor(() => expect(record.puts.length).toBe(puts + 1), wait);
    expect(record.presigns.at(-1)).toMatchObject({ path: "images/b", type: "image/jpeg" });
    await waitFor(() => expect(ops(record).at(-1)).toEqual(["put images/b.jpg"]), wait);

    await waitFor(() => expect(row("images/b.png") ?? row("images/b.jpg")).not.toBeNull(), wait);
    await user.click(within(row("images/first.png")).getByRole("checkbox"));
    await user.click(within(row("videos/v.mp4")).getByRole("checkbox"));
    expect(screen.getByText("2 selected")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove selected" }));
    await waitFor(() => expect(ops(record).at(-1)).toEqual(["remove images/first.png", "remove videos/v.mp4"]), wait);
    await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(1), wait);
    expect(screen.queryByText(/selected/)).toBeNull();
  } finally {
    await h.clearFaults(ref.id);
  }
});

it("a draft commits files as they finish; discard() empties the queue", async () => {
  const ref = await folder("album");
  const { record, c, user } = setup();
  const handle = createRef<MediaFolderEditorHandle>();
  const { container } = render(<MediaFolderEditor client={c} item={ref} commit="auto" ref={handle} />);
  await screen.findByText("Drop files here, or click to choose");
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, [png("1.png", 11), png("2.png", 12)]);
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2), wait);
  expect(record.commits.flat().map((op) => op.path)).toEqual(["images/1.png", "images/2.png"]);
  expect(screen.queryByRole("button", { name: /^Add \d/ })).toBeNull();

  // The upload API refuses this item's next uploads.
  await h.faults([{ item: ref.id, fault: "api", path: "/media/upload/presign", method: "POST", status: 429, code: "rate_limited", error: "slow down", retry_after: 30 }]);
  try {
    await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-add] input")!, [png("3.png", 13)]);
    expect(await screen.findByText("Uploads paused", undefined, wait)).toBeInTheDocument();
    expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(1);
    handle.current!.discard();
    await waitFor(() => expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(0), wait);
  } finally {
    await h.clearFaults(ref.id);
  }
});

it("reports an upload the worker fails after the editor opened, once", async () => {
  const ref = await folder("album");
  const { c } = setup();
  const onError = vi.fn();
  // One photo the worker already refused; another it refuses once the editor shows it processing.
  const old = await c.media.upload(garbage("old.png", "image/png", 21), { ref, path: "images/old.png" });
  await c.media.commit(ref, [{ op: "put", path: old.path, blob: old.blob }]);
  await waitFor(async () => expect((await c.media.read(ref, { editor: true })).files.find((f) => f.path === "images/old.png")?.failed).toBeDefined(), wait);
  await h.faults([{ item: ref.id, fault: "hold", from: "worker" }]);
  try {
    const up = await c.media.upload(garbage("new.png", "image/png", 22), { ref, path: "images/new.png" });
    await c.media.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    const { container } = render(<MediaFolderEditor client={c} item={ref} poll={100} onError={onError} />);
    await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2), wait);
    expect(screen.getByText("Processing…")).toBeInTheDocument();
    expect(onError).not.toHaveBeenCalled();
    await h.clearFaults(ref.id);
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1), wait);
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: expect.any(String) }), { operation: "folder.process", file: "new.png" });
    await waitFor(() => expect(screen.getAllByText("Not processed")).toHaveLength(2), wait);
    await new Promise((r) => setTimeout(r, 300));
    expect(onError).toHaveBeenCalledTimes(1);
  } finally {
    await h.clearFaults(ref.id);
  }
});

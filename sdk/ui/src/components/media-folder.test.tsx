// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef } from "react";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient } from "../../test/fake.js";
import type { UploadRule } from "../client/generated/wire.js";
import { MediaFolderEditor, type MediaFolderEditorHandle } from "../index.js";
import { ContentKitProvider } from "../react/index.js";

const MiB = 1 << 20;
const item = { kind: "post", id: "0192f000-0000-7000-8000-000000000011" };
const images: UploadRule = { path: "images/{name}", types: ["image/png", "image/jpeg"], max_bytes: 25 * MiB, max: 3 };
const videos: UploadRule = { path: "videos/{name}", types: ["video/mp4"], max_bytes: 1024 * MiB, min_aspect: 1 / 2.4, max_aspect: 2.4 };
const png = (name: string, seed = 1) => new File([bytes(200, seed)], name, { type: "image/png" });

function setup() {
  const s = new FakeServer();
  s.rules.set("post", [images, videos, { path: "inline/{name}", types: ["image/png"], max_bytes: MiB, named: true }]);
  const client = fakeClient(s);
  const onError = vi.fn();
  const user = userEvent.setup({ applyAccept: false });
  return { s, client, onError, user };
}

const ops = (s: FakeServer) => s.commits.map((c) => c.map((op) => `${op.op} ${op.path}${op.to ? ` → ${op.to}` : ""}`));

it("screens files against the kind's rules, uploads the rest and adds them in queue order", async () => {
  const { s, client, onError, user } = setup();
  const { container } = render(
    <ContentKitProvider client={client} onError={onError}>
      <MediaFolderEditor item={item} />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("Drop files here, or click to choose")).toBeInTheDocument();
  expect(screen.getByText("Images up to 25 MB, at most 3 · Videos up to 1 GB · video shapes from 1:2.4 to 2.4:1")).toBeInTheDocument();
  const input = container.querySelector<HTMLInputElement>("[data-ckui=folder-drop] input[type=file]")!;
  expect(input.accept).toContain("video/mp4");

  await user.upload(input, [png("b.png", 1), png("a.png", 2), new File([bytes(10)], "c.gif", { type: "image/gif" })]);
  const refused = await screen.findByRole("alert");
  expect(refused).toHaveTextContent("c.gif: This file type isn't supported here.");
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "type_not_allowed" }), { operation: "upload", file: "c.gif" });

  await waitFor(() => expect(screen.getAllByText("Uploaded")).toHaveLength(2));
  expect(s.presigns.map((p) => p.path)).toEqual(["images/b.png", "images/a.png"]);
  await user.click(screen.getByRole("button", { name: "Add 2" }));
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2));
  expect(ops(s)).toEqual([["put images/b.png", "put images/a.png"]]);
  expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(0);
  expect([...container.querySelectorAll("[data-ckui=upload-name]")].map((n) => n.textContent)).toEqual(["b.png", "a.png"]);

  // The cap counts files already there: one more image fits, then none.
  await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-add] input")!, [png("d.png", 3), png("e.png", 4)]);
  expect(await screen.findByText(/e\.png/)).toBeInTheDocument();
  expect(screen.getByRole("alert")).toHaveTextContent("This item holds at most 3 of these files. Remove some first.");
  // A name already taken is numbered.
  await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-add] input")!, [new File([bytes(9, 5)], "v.mp4", { type: "video/mp4" }), new File([bytes(9, 6)], "v.mp4", { type: "video/mp4" })]);
  await waitFor(() => expect(s.presigns.map((p) => p.path).slice(-2).sort()).toEqual(["videos/v-2.mp4", "videos/v.mp4"]));
});

it("renames, crops, replaces and removes uploads, and shows each one's thumbnail and state", async () => {
  const { s, client, user } = setup();
  s.seed(item, [
    { path: "images/a.png", type: "image/png", size: 3, w: 4000, h: 3000 },
    { path: "images/b.png", type: "image/png", size: 3, w: 4000, h: 3000, edit: { rotate: 90 } },
    { path: "low-res/a.webp", type: "image/webp", size: 2, w: 600, from: "images/a.png", upload: false },
    { path: "videos/v.mp4", type: "video/mp4", size: 9, w: 1920, h: 1080, pending: ["hls"], progress: { phase: "encoding", percent: 40, segments_done: 4, segments_total: 10, at: Date.now() } },
  ]);
  const { container } = render(<MediaFolderEditor client={client} item={item} confirmRemove={false} />);
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(3));
  const row = (path: string) => container.querySelector<HTMLElement>(`[data-ckui=upload-row][data-path="${path}"]`)!;
  expect(row("images/a.png").querySelector("img")).toHaveAttribute("src", expect.stringContaining("fake://cdn/private/low-res/a.webp"));
  expect(within(row("images/b.png")).getByText("Edited")).toBeInTheDocument();
  expect(within(row("videos/v.mp4")).getByText(/Encoding · segment 4 \/ 10/)).toBeInTheDocument();
  expect(container.querySelectorAll("[data-ckui=folder-group] h4")[0]).toHaveTextContent("Images · 2 / 3");

  await user.click(within(row("images/a.png")).getByRole("button", { name: "Rename" }));
  const name = screen.getByRole("textbox", { name: "New name for a.png" });
  await user.clear(name);
  await user.type(name, "first{Enter}");
  await waitFor(() => expect(ops(s).at(-1)).toEqual(["rename images/a.png → images/first"]));
  await waitFor(() => expect(row("images/first.png")).not.toBeNull());

  await user.click(within(row("images/b.png")).getByRole("button", { name: "Crop and rotate" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByRole("button", { name: "Original" })).toHaveAttribute("aria-pressed", "true");
  await user.click(within(dialog).getByRole("button", { name: "1:1" }));
  await user.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(s.commits.at(-1)).toEqual([{ op: "edit", path: "images/b.png", edit: { crop: { x: 500, y: 0, w: 3000, h: 3000 } } }]);

  const puts = s.puts.length;
  await user.click(within(row("images/b.png")).getByRole("button", { name: "Replace" }));
  fireEvent.change(container.querySelector<HTMLInputElement>("input[aria-hidden]")!, { target: { files: [new File([bytes(50, 9)], "new.jpg", { type: "image/jpeg" })] } });
  await waitFor(() => expect(s.puts.length).toBe(puts + 1));
  expect(s.presigns.at(-1)).toMatchObject({ path: "images/b", type: "image/jpeg" });
  await waitFor(() => expect(ops(s).at(-1)).toEqual(["put images/b.jpg"]));

  await waitFor(() => expect(row("images/b.png") ?? row("images/b.jpg")).not.toBeNull());
  await user.click(within(row("images/first.png")).getByRole("checkbox"));
  await user.click(within(row("videos/v.mp4")).getByRole("checkbox"));
  expect(screen.getByText("2 selected")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Remove selected" }));
  await waitFor(() => expect(ops(s).at(-1)).toEqual(["remove images/first.png", "remove videos/v.mp4"]));
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(1));
  expect(screen.queryByText(/selected/)).toBeNull();
});

it("a draft commits files as they finish; discard() empties the queue", async () => {
  const { s, client, user } = setup();
  const handle = createRef<MediaFolderEditorHandle>();
  const { container } = render(<MediaFolderEditor client={client} item={item} commit="auto" ref={handle} />);
  await screen.findByText("Drop files here, or click to choose");
  await user.upload(container.querySelector<HTMLInputElement>("input[type=file]")!, [png("1.png", 1), png("2.png", 2)]);
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2));
  expect(s.commits.flat().map((op) => op.path)).toEqual(["images/1.png", "images/2.png"]);
  expect(screen.queryByRole("button", { name: /^Add \d/ })).toBeNull();

  s.refuse = { status: 429, code: "rate_limited", error: "slow down", retry_after: 30 };
  await user.upload(container.querySelector<HTMLInputElement>("[data-ckui=folder-add] input")!, [png("3.png", 3)]);
  expect(await screen.findByText("Uploads paused")).toBeInTheDocument();
  expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(1);
  handle.current!.discard();
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=queue-row]")).toHaveLength(0));
});

it("reports an upload the worker fails after the editor opened, once", async () => {
  const { s, client } = setup();
  const onError = vi.fn();
  s.seed(item, [
    { path: "images/old.png", type: "image/png", size: 3, failed: { of: "low", message: "bad", code: "image_unreadable", details: { type: "image/png" } } },
    { path: "images/new.png", type: "image/png", size: 3, pending: ["low"] },
  ]);
  const { container } = render(<MediaFolderEditor client={client} item={item} poll={30} onError={onError} />);
  await waitFor(() => expect(container.querySelectorAll("[data-ckui=upload-row]")).toHaveLength(2));
  expect(screen.getByText("Processing…")).toBeInTheDocument();
  s.seed(item, [
    { path: "images/old.png", type: "image/png", size: 3, failed: { of: "low", message: "bad", code: "image_unreadable", details: { type: "image/png" } } },
    { path: "images/new.png", type: "image/png", size: 3, failed: { of: "low", message: "too small", code: "image_too_small", details: { width: 10, min_width: 400 } } },
  ]);
  await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "image_too_small" }), { operation: "folder.process", file: "new.png" });
  expect(screen.getAllByText("Not processed")).toHaveLength(2);
  expect(screen.getByText(/This image is 10 px wide after cropping/)).toBeInTheDocument();
  await new Promise((r) => setTimeout(r, 120));
  expect(onError).toHaveBeenCalledTimes(1);
});

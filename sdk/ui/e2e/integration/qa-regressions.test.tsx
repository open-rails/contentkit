// @vitest-environment jsdom
import "../../src/test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, it } from "vitest";
import type { ContentKitClient, RefBody } from "../../src/client/index.js";
import { ContentManager, MediaFolderEditor, ReaderStats, StatsDashboard, VersionsEditor } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, fixture, harness, item, png, recorder, wait } from "./setup.js";

const h = harness();
let cfg: Config;
let staff: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  staff = await h.user({ role: "staff" });
});

it("the content manager creates video content and opens an added version as a draft with video uploads", async () => {
  const c = client(h, cfg, staff);
  const user = userEvent.setup();
  const { container } = render(<ContentManager client={c} kind="video" languages={["en"]} />);
  await user.click(await screen.findByRole("button", { name: "New item" }, wait));
  await user.type(screen.getByRole("textbox", { name: "Title" }), "QA video");
  await user.click(screen.getByRole("button", { name: "Create draft" }));
  const versions = await waitFor(() => {
    const section = container.querySelector<HTMLElement>('[data-ckui="versions-editor"]');
    expect(section).not.toBeNull();
    return section!;
  }, wait);
  expect(versions.querySelectorAll('[data-ckui="version-row"]')).toHaveLength(1);
  await user.click(within(versions).getByRole("combobox", { name: "Version language" }));
  await user.click(await screen.findByRole("option", { name: "German" }));
  await user.click(within(versions).getByRole("button", { name: "Add version" }));
  await waitFor(() => expect(versions.querySelectorAll('[data-ckui="version-row"]')).toHaveLength(2), wait);
  const added = versions.querySelectorAll<HTMLElement>('[data-ckui="version-row"]')[1]!;
  expect(added.querySelector('[data-slot="badge"]')).toHaveTextContent("Draft");
  expect(within(added).getByRole("switch", { name: "Draft" })).toBeChecked();
  expect(within(added).getByRole("button", { name: "Close" })).toHaveAttribute("aria-expanded", "true");
  const source = await within(added).findByLabelText("Video file", undefined, wait);
  expect(source).toHaveAttribute("accept", expect.stringContaining("video/mp4"));
  expect(within(added).getByLabelText("Subtitle file (.vtt, .srt)")).toBeInTheDocument();
  await user.click(within(added).getByRole("combobox", { name: "Language" }));
  expect(await screen.findByRole("option", { name: "German" })).toBeInTheDocument();
});

async function pending(c: ContentKitClient, ref: RefBody, name: string, seed: number) {
  const uploaded = await c.media.upload(png(name, seed), { ref, path: `originals/${name}` });
  await c.media.commit(ref, [{ op: "put", path: uploaded.path, blob: uploaded.blob, create_id: crypto.randomUUID(), unattached: true }]);
}

it("an earlier visit's uploads return, reserve their names and are added beside new uploads", async () => {
  const owner = await h.user();
  const ref = await item(h, "gallery", owner);
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  await pending(c, ref, "a.png", 80);
  record.commits.length = 0;
  record.presigns.length = 0;
  const user = userEvent.setup();
  const { container } = render(<MediaFolderEditor item={ref} client={c} poll={false} />);
  await waitFor(() => expect(container.querySelector('[data-ckui="pending-row"]')).toHaveTextContent("a.png"), wait);
  await user.upload(container.querySelector<HTMLInputElement>('[data-ckui="folder-add"] input')!, png("a.png", 81));
  await waitFor(() => expect(record.presigns.map((p) => p.path)).toEqual(["originals/a-2.png"]), wait);
  const add = await screen.findByRole("button", { name: "Add 2" }, wait);
  await waitFor(() => expect(add).toBeEnabled(), wait);
  await user.click(add);
  await waitFor(() => expect(container.querySelectorAll('[data-ckui="upload-row"]')).toHaveLength(2), wait);
  expect(record.commits[0]!.map((op) => [op.op, op.path])).toEqual([["put", "originals/a-2.png"], ["attach", "originals/a.png"]]);
  expect(container.querySelector('[data-ckui="pending-row"]')).toBeNull();
});

it("a draft adds earlier unattached uploads automatically", async () => {
  const owner = await h.user();
  const ref = await item(h, "gallery", owner);
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  await pending(c, ref, "b.png", 82);
  record.commits.length = 0;
  const { container } = render(<MediaFolderEditor item={ref} client={c} commit="auto" poll={false} />);
  await waitFor(() => expect(container.querySelectorAll('[data-ckui="upload-row"]')).toHaveLength(1), wait);
  expect(record.commits[0]).toEqual([expect.objectContaining({ op: "attach", path: "originals/b.png" })]);
});

it("earlier unattached uploads count toward the folder's upload cap", async () => {
  const owner = await h.user();
  const ref = await item(h, "note", owner);
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  await pending(c, ref, "a.png", 83);
  await pending(c, ref, "b.png", 84);
  record.presigns.length = 0;
  const user = userEvent.setup();
  const { container } = render(<MediaFolderEditor item={ref} client={c} poll={false} />);
  await screen.findByRole("button", { name: "Add 2" }, wait);
  await user.upload(container.querySelector<HTMLInputElement>('[data-ckui="folder-add"] input')!, png("c.png", 85));
  expect(await screen.findByRole("alert")).toHaveTextContent("This item holds at most 2 of these files. Remove some first.");
  expect(record.presigns).toHaveLength(0);
});

it("staff statistics expose unavailable measured engagement and still rank comments; readers see their own totals", async () => {
  const c = client(h, cfg, staff);
  const title = `QA statistics ${crypto.randomUUID()}`;
  const blog = await c.contents.create({ kind: "blog", text: { en: { title } } });
  await c.contents.publish(blog.id);
  const author = await h.user();
  const reader = client(h, cfg, author);
  await reader.comments.create({ type: "contents", id: blog.id }, { body: "A reader comment." });
  const user = userEvent.setup();
  const dashboard = render(<StatsDashboard client={c} kinds={["blog"]} />);
  await waitFor(() => expect(screen.getAllByText("Measured engagement is not available on this site.")).toHaveLength(2), wait);
  await user.click(screen.getByRole("combobox", { name: "Ranked by" }));
  await user.click(await screen.findByRole("option", { name: "Comments" }));
  expect(await screen.findByRole("link", { name: title }, wait)).toBeInTheDocument();
  dashboard.unmount();
  const own = await reader.stats.mine();
  expect(own.comments).toBe(1);
  render(<ContentKitProvider client={reader} viewer={author.id}><ReaderStats /></ContentKitProvider>);
  await waitFor(() => expect(screen.getByText("Comments").nextElementSibling).toHaveTextContent("1"), wait);
});

it("a video version exposes its poster once a source has been uploaded", async () => {
  const c = client(h, cfg, staff);
  const video = await c.contents.create({ kind: "video", text: { en: { title: "Source and poster" } } });
  const ref = { kind: video.kind, id: video.id, version: video.versions[0]!.id };
  await c.media.put(fixture("portrait.mp4", "video/mp4"), { ref, path: "source", wait: false });
  const user = userEvent.setup();
  const { container } = render(<VersionsEditor content={video} client={c} />);
  await user.click(await screen.findByRole("button", { name: "Edit" }, wait));
  expect(await screen.findByRole("button", { name: "Replace video" }, wait)).toBeInTheDocument();
  expect(await screen.findByRole("button", { name: "Choose poster" }, wait)).toBeInTheDocument();
  expect(container.querySelector('[data-ckui="media-subtitles"]')).not.toBeNull();
});

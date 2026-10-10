// @vitest-environment jsdom
import "../../src/test/dom.js";
import { File as NodeFile } from "node:buffer";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, expect, it, vi } from "vitest";
import { createContentKitClient, fetchTransport, type RefBody } from "../../src/client/index.js";
import { MediaGallery, MediaReadinessNotice, VideoPosterPicker } from "../../src/index.js";
import { ContentKitProvider } from "../../src/react/index.js";
import { bytes } from "../support/bytes.js";
import type { Config, TestUser } from "../support/harness.js";
import { client, fixture, harness, item, png, recorder, wait } from "./setup.js";

const h = harness();
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  owner = await h.user();
});

URL.createObjectURL ??= () => "blob:frame";
URL.revokeObjectURL ??= () => {};

async function own(kind: string): Promise<RefBody> {
  const { kind: k, id } = await item(h, kind, owner);
  return { kind: k, id };
}

/** Bytes no decoder reads, declared as type: the worker refuses them. */
const garbage = (name: string, type: string, seed = 1) => new NodeFile([bytes(4096, seed)], name, { type }) as unknown as File;

it("MediaReadinessNotice shows processing, then the failed files, then nothing", async () => {
  const post = await own("album");
  const c = client(h, cfg, owner);
  // The worker is held on the item while it would process a clip and a photo it cannot read.
  await h.faults([{ item: post.id, fault: "hold", from: "worker" }]);
  try {
    const a = await c.media.upload(garbage("a.mp4", "video/mp4", 1), { ref: post, path: "videos/a.mp4" });
    const b = await c.media.upload(garbage("b.png", "image/png", 2), { ref: post, path: "images/b.png" });
    await c.media.commit(post, [{ op: "put", path: a.path, blob: a.blob }, { op: "put", path: b.path, blob: b.blob }]);
    const { container, rerender } = render(<MediaReadinessNotice client={c} item={post} />);
    expect(await screen.findByText("Processing media", undefined, wait)).toBeInTheDocument();
    await h.clearFaults(post.id);
    rerender(<MediaReadinessNotice client={c} item={post} prefix="" />);
    expect(await screen.findByText(/^2 files couldn't be processed: (a\.mp4, b\.png|b\.png, a\.mp4)$/, undefined, wait)).toBeInTheDocument();
    // Removing them leaves nothing to report.
    await act(async () => void (await c.media.commit(post, [{ op: "remove", path: "videos/a.mp4" }, { op: "remove", path: "images/b.png" }])));
    await waitFor(() => expect(container.querySelector("[data-ckui=readiness]")).toBeNull(), wait);
  } finally {
    await h.clearFaults(post.id);
  }
});

it("MediaGallery item reads through the client and builds HLS bases on its media mount", async () => {
  const post = await own("album");
  const owned = client(h, cfg, owner);
  await owned.media.put(png("1.png", 31, 400, 300), { ref: post, path: "images/1.png" });
  await owned.media.put(png("2.png", 32, 300, 400), { ref: post, path: "images/2.png" });
  await owned.media.waitFor(post, "images/1.png", wait);
  await owned.media.waitFor(post, "images/2.png", wait);
  // A client whose reads this test sees.
  const reads: URL[] = [];
  const c = createContentKitClient({
    baseUrl: `${h.origin}${cfg.api}`,
    token: () => owner.access_token,
    fetch: (input, init) => {
      const url = new URL(String(input));
      if (/\/media\/album\//.test(url.pathname)) reads.push(url);
      return fetch(input, init);
    },
    media: { transport: fetchTransport },
  });
  render(
    <ContentKitProvider client={c}>
      <MediaGallery item={post} prefix="large/" storageKey={null} />
    </ContentKitProvider>,
  );
  expect(await screen.findByText("1 / 2", undefined, wait)).toBeInTheDocument();
  expect(reads.at(-1)!.searchParams.get("prefix")).toBe("large/");
  expect(screen.getByRole("img", { name: "Image 1" })).toHaveAttribute("src", expect.stringContaining(`${cfg.media}/v1/${cfg.namespace}/album/${post.id}/private/`));
  expect(c.media.hlsBase(post, "hls/")).toBe(`${h.origin}${cfg.api}/media/album/${post.id}/hls/hls/`);
});

it("VideoPosterPicker takes frames from another video and uploads the chosen one as the cover image", async () => {
  const series = await own("channel");
  const video = await own("video");
  const record = recorder();
  const c = client(h, cfg, owner, { record });
  await c.media.put(fixture("clip.mp4", "video/mp4", "v.mp4"), { ref: video, path: "source" });
  await c.media.waitFor(video, "source", wait);
  record.frames.length = record.presigns.length = record.commits.length = 0;
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(<VideoPosterPicker client={c} open onOpenChange={() => {}} item={series} path="cover" frames={{ item: video }} aspect="16:9" onChange={onChange} />);
  const dialog = await screen.findByRole("dialog");
  await waitFor(() => expect(record.frames.length).toBeGreaterThan(1), wait);
  expect(within(dialog).queryByRole("button", { name: "Automatic" })).toBeNull();
  await user.click(await within(dialog).findByRole("button", { name: "Use this frame" }, wait));
  await waitFor(() => expect(onChange).toHaveBeenCalledOnce(), wait);
  // The full-size frame becomes an upload of the cover: no frame op on another item.
  expect(record.frames.some((f) => f.endsWith("@null"))).toBe(true);
  expect(record.presigns.at(-1)).toMatchObject({ path: "cover", type: "image/jpeg" });
  expect(record.commits.at(-1)).toEqual([{ op: "put", path: "cover.jpg", blob: expect.stringMatching(/^u-/) }]);
  expect(record.commits.flat().some((op) => op.op === "frame")).toBe(false);
});

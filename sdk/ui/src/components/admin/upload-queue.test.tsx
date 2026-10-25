// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import { ContentKitError } from "../../client/errors.js";
import { ContentKitProvider } from "../../react/provider.js";
import { UploadQueueBody } from "./upload-queue.js";

const ref = { kind: "gallery", id: "01900000-0000-7000-8000-000000000001", version: "01900000-0000-7000-8000-000000000002" };
const file = () => new File(["archive"], "pages.zip", { type: "application/zip", lastModified: 1 });
const storageKey = (client: ReturnType<typeof createContentKitClient>, viewer: string) => `test.uploads:${encodeURIComponent(JSON.stringify([client.url("upload"), viewer]))}`;
afterEach(() => localStorage.clear());

function fixture(baseUrl = "https://catalog.test") {
  const client = createContentKitClient({ baseUrl });
  const upload = vi.spyOn(client.media, "upload").mockResolvedValue({ path: "import/pages.zip", blob: "staged", type: "application/zip", size: 7, exists: false });
  vi.spyOn(client.media, "read").mockResolvedValue({ access: "full", expires: 0, total: 0, offset: 0, limit: 0, files: [] });
  const create = vi.fn(async () => ref);
  const error = vi.fn();
  const content = (viewer: string | undefined) => <ContentKitProvider client={client} viewer={viewer}>
    <UploadQueueBody client={client} createItem={create} storageKey="test.uploads" path="import/pages.zip" onError={error} />
  </ContentKitProvider>;
  return { client, upload, create, error, content };
}

it("replays the exact generic commit after an uncertain response and reload", async () => {
  const f = fixture();
  const commit = vi.spyOn(f.client.media, "commit").mockImplementationOnce(async (target, ops, options) => {
    options?.onState?.({ ref: target, ops, operation_id: options.operationID! });
    throw new ContentKitError("network", "Lost response");
  }).mockResolvedValue([{ path: "import/pages.zip", type: "application/zip" }, { path: "pages/001.png", type: "image/png", pending: ["low"] }]);
  const user = userEvent.setup();
  const first = render(f.content("alice"));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(first.container.querySelector('[data-status="failed"]')).not.toBeNull());
  const saved = localStorage.getItem(storageKey(f.client, "alice"))!;
  expect(saved).toContain('"operation_id":');
  expect(saved).not.toMatch(/authorization|https:\/\//i);
  const original = commit.mock.calls[0]!;
  first.unmount();
  const second = render(f.content("alice"));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(second.container.querySelector('[data-status="done"]')).not.toBeNull());
  expect(f.create).toHaveBeenCalledTimes(1);
  expect(f.upload).toHaveBeenCalledTimes(1);
  expect(commit.mock.calls[1]![1]).toEqual(original[1]);
  expect(commit.mock.calls[1]![2]?.operationID).toBe(original[2]?.operationID);
  expect(localStorage.getItem(storageKey(f.client, "alice"))).toBeNull();
});

it("does not restore another account's pending commit", async () => {
  const f = fixture();
  const commit = vi.spyOn(f.client.media, "commit").mockImplementationOnce(async (target, ops, options) => {
    options?.onState?.({ ref: target, ops, operation_id: options.operationID! });
    throw new ContentKitError("network", "Lost response");
  }).mockResolvedValue([{ path: "import/pages.zip", type: "application/zip" }]);
  const user = userEvent.setup();
  const view = render(f.content("alice"));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(view.container.querySelector('[data-status="failed"]')).not.toBeNull());
  const aliceSaved = localStorage.getItem(storageKey(f.client, "alice"));
  view.rerender(f.content("bob"));
  expect(view.container.querySelector('[data-ckui="upload-entry"]')).toBeNull();
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(view.container.querySelector('[data-status="done"]')).not.toBeNull());
  expect(f.create).toHaveBeenCalledTimes(2);
  expect(f.upload).toHaveBeenCalledTimes(2);
  expect(commit.mock.calls[1]![2]?.operationID).not.toBe(commit.mock.calls[0]![2]?.operationID);
  expect(localStorage.getItem(storageKey(f.client, "alice"))).toBe(aliceSaved);
});

it("aborts the old account's transfer when the viewer changes", async () => {
  const f = fixture();
  let signal: AbortSignal | undefined;
  f.upload.mockImplementationOnce(async (_file, options) => {
    signal = options.signal;
    return new Promise((_resolve, reject) => options.signal?.addEventListener("abort", () => reject(new ContentKitError("aborted", "Account changed")), { once: true }));
  });
  const commit = vi.spyOn(f.client.media, "commit");
  const user = userEvent.setup();
  const view = render(f.content("alice"));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(f.upload).toHaveBeenCalledOnce());
  view.rerender(f.content("bob"));
  expect(signal?.aborted).toBe(true);
  expect(commit).not.toHaveBeenCalled();
  expect(within(view.container).queryByText("pages.zip")).toBeNull();
});

it("keeps an uncertain commit in memory but never persists it without a known account", async () => {
  const f = fixture();
  const commit = vi.spyOn(f.client.media, "commit").mockImplementationOnce(async (target, ops, options) => {
    options?.onState?.({ ref: target, ops, operation_id: options.operationID! });
    throw new ContentKitError("network", "Lost response");
  }).mockResolvedValue([{ path: "import/pages.zip", type: "application/zip", pending: ["import"] }]);
  const user = userEvent.setup();
  const view = render(f.content(undefined));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(view.container.querySelector('[data-status="failed"]')).not.toBeNull());
  expect(localStorage.length).toBe(0);
  await user.click(within(view.container.querySelector<HTMLElement>('[data-ckui="upload-entry"]')!).getByRole("button", { name: "Try again" }));
  await waitFor(() => expect(view.container.querySelector('[data-status="done"]')).not.toBeNull());
  expect(f.upload).toHaveBeenCalledOnce();
  expect(commit.mock.calls[1]![2]?.operationID).toBe(commit.mock.calls[0]![2]?.operationID);
  expect(localStorage.length).toBe(0);
});

it("shows actual transferred byte totals separately from server processing", async () => {
  const f = fixture();
  f.upload.mockImplementationOnce(async (_file, options) => {
    options.onProgress?.({ phase: "uploading", loaded: 5 * 1024 * 1024, total: 30 * 1024 * 1024 });
    return new Promise((_resolve, reject) => options.signal?.addEventListener("abort", () => reject(new ContentKitError("aborted", "Closed")), { once: true }));
  });
  const user = userEvent.setup();
  const view = render(f.content("alice"));
  await user.upload(screen.getByLabelText("Add files"), file());
  await waitFor(() => expect(view.container.querySelector('[data-ckui="upload-transfer"]')).toHaveTextContent("5 MB / 30 MB"));
  expect(view.container.querySelector('[data-ckui="media-processing"]')).toBeNull();
  view.unmount();
});

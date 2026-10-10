import { File as NodeFile } from "node:buffer";
import { beforeAll, expect, it, vi } from "vitest";
import { UploadQueue, createContentKitClient, type Op, type QueueSnapshot, type RefBody } from "../../src/client/index.js";
import { bytes } from "../support/bytes.js";
import type { Config, TestUser } from "../support/harness.js";
import { Accounts, client, harness, item, png, recorder, wait, type Recorder } from "./setup.js";

const path = "originals/{name}";
const h = harness();
const accounts = new Accounts(h);
let cfg: Config;
let owner: TestUser;

beforeAll(async () => {
  cfg = await h.config();
  await accounts.load("owner");
  owner = accounts.get("owner");
});

const gallery = () => item(h, "gallery", owner);
const media = (rec?: Recorder, onUpload = false, retries?: number) => client(h, cfg, owner, { record: rec, onUpload, media: { retries } }).media;
const gif = () => new NodeFile([bytes(10)], "x.gif", { type: "" }) as unknown as File;

function until(q: UploadQueue, ok: (s: QueueSnapshot) => boolean): Promise<QueueSnapshot> {
  return new Promise((resolve) => {
    const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
    const off = q.subscribe(check);
    check();
  });
}

/** A client whose /commit responses are lost (after the server applied them) while lose() says so. */
function losing(rec: Recorder, lose: () => boolean, onUpload = false, retries?: number) {
  return createContentKitClient({
    baseUrl: `${h.origin}${cfg.api}`,
    mounts: onUpload ? { upload: `${h.origin}${cfg.on_upload_api}/media/upload` } : undefined,
    token: () => owner.access_token,
    media: { retries, retryDelay: () => 0, transport: rec.transport },
    fetch: async (input, init) => {
      const response = await rec.fetch(input, init);
      if (String(input).endsWith("/commit") && response.ok && lose()) throw new TypeError("response lost");
      return response;
    },
  }).media;
}

const uploads = async (ref: RefBody) => (await media().read(ref, { editor: true })).files.filter((f) => f.upload);

it("uploads, keeps the arranged order and commits puts appended in it", async () => {
  const ref = await gallery();
  await media().put(png("0.png", 10), { ref, path: "originals/0.png" });
  const rec = recorder();
  const q = new UploadQueue(media(rec), { ref, path });
  const [a, b, c] = q.add([png("a.png", 1), png("b.png", 2), png("c.png", 3)]);
  expect(a!.path).toBe("originals/a.png");
  await until(q, (x) => x.ready);
  q.move(c!.id, 0);
  q.update(b!.id, { path: "originals/02.png", meta: { alt: "b" } });
  const files = await q.commit();
  expect(files.map((f) => f.path)).toEqual(["originals/0.png", "originals/c.png", "originals/a.png", "originals/02.png"]);
  expect(rec.commits[0]!.map((op) => [op.op, op.path, op.index])).toEqual([
    ["put", "originals/c.png", 2 ** 31 - 1],
    ["put", "originals/a.png", 2 ** 31 - 1],
    ["put", "originals/02.png", 2 ** 31 - 1],
  ]);
  expect(rec.commits[0]![2]).toMatchObject({ meta: { alt: "b" }, blob: expect.stringMatching(/^u-/) });
  expect(rec.commits[0]!.map((op) => op.create_id)).toEqual([c!.id, a!.id, b!.id]);
  expect(q.getSnapshot().items.map((i) => i.status)).toEqual(["committed", "committed", "committed"]);
  expect(a!.status).toBe("queued"); // snapshots are immutable
});

it("blocks on a rate limit before starting the next file, and resumes on start()", async () => {
  const ref = await gallery();
  const rec = recorder();
  await h.faults([{ item: ref.id, fault: "api", path: "/media/upload/presign", status: 429, code: "rate_limited", error: "slow down", retry_after: 30 }]);
  const q = new UploadQueue(media(rec), { ref, path });
  try {
    q.add([png("a.png", 1), png("b.png", 2), png("c.png", 3)]);
    const blocked = await until(q, (x) => !!x.blocked && x.items.every((i) => i.status === "queued"));
    expect(blocked.blocked!.retryAfter).toBe(30);
    expect(rec.presigns.length).toBeLessThanOrEqual(2); // the concurrent pair; the third never started
  } finally {
    await h.clearFaults(ref.id);
  }
  q.start();
  await until(q, (x) => x.ready);
  expect(rec.puts.length).toBe(3);
  q.dispose();
});

it("marks a file failed on its own refusal and retries it", async () => {
  const ref = await gallery();
  const rec = recorder();
  const q = new UploadQueue(media(rec), { ref, path });
  const [bad] = q.add([gif()]);
  await until(q, (x) => x.items[0]!.status === "failed");
  expect(q.getSnapshot().blocked).toBeUndefined();
  q.remove(bad!.id);
  expect(q.getSnapshot().items).toEqual([]);
  expect(rec.puts.length).toBe(0);
});

it("retains a stale file's replacement batch across lost responses and a later queue retry", async () => {
  const ref = await gallery();
  // b's bytes are placed at another path first, so b's upload is an exists offer;
  // removing that path before the commit makes the offer stale.
  const b2 = png("b.png", 2);
  const keep = await media().put(b2, { ref, path: "originals/keep.png" });
  const rec = recorder();
  let loseResponses = true;
  const q = new UploadQueue(losing(rec, () => loseResponses, false, 1), { ref, path });
  const [, b] = q.add([png("a.png", 1), b2]);
  const snap = await until(q, (x) => x.ready);
  expect(snap.items.find((i) => i.id === b!.id)!.result).toMatchObject({ exists: true });
  await media().commit(ref, [{ op: "remove", path: keep.path }]);
  await expect(q.commit()).rejects.toMatchObject({ code: "network" });
  const replacement = rec.commitRequests.at(-1)!;
  expect(q.getSnapshot().items[1]!.result!.blob).toBe(replacement.ops[1]!.blob);
  loseResponses = false;
  const files = await q.commit();
  expect(rec.commitRequests.at(-1)).toEqual(replacement);
  expect(files.map((f) => f.path)).toEqual(["originals/a.png", "originals/b.png"]);
  // Applied once: a's PUT, then only b again.
  expect((await uploads(ref)).map((f) => f.path)).toEqual(["originals/a.png", "originals/b.png"]);
  expect(rec.puts.length).toBe(2);
  expect(replacement.ops[1]!.create_id).toBe(b!.id);
  expect(q.getSnapshot().items.every((i) => i.status === "committed")).toBe(true);
  q.dispose();
});

it("does not remove an existing file when a cancelled staging create is rejected", async () => {
  const ref = await gallery();
  const existing = await media().put(png("a.png", 7), { ref, path: "originals/a.png" });
  const rec = recorder();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  // The staging create reaches the server only after the item is removed.
  const c = createContentKitClient({
    baseUrl: `${h.origin}${cfg.api}`,
    mounts: { upload: `${h.origin}${cfg.on_upload_api}/media/upload` },
    token: () => owner.access_token,
    media: { retryDelay: () => 0, transport: rec.transport },
    fetch: async (input, init) => {
      if (String(input).endsWith("/commit")) {
        const body = JSON.parse(String(init?.body)) as { ops: Op[] };
        if (body.ops[0]?.op === "put") await gate;
      }
      return rec.fetch(input, init);
    },
  }).media;
  const q = new UploadQueue(c, { ref, path });
  const [up] = q.add([png("a.png", 1)]);
  await until(q, (x) => x.ready);
  const pending = q.commit().catch((e) => e);
  q.remove(up!.id);
  release();
  await pending;
  await vi.waitFor(() => expect(rec.commits.length).toBeGreaterThanOrEqual(1), wait);
  await new Promise((r) => setTimeout(r, 500));
  expect(rec.commits.flat().filter((op) => op.op === "remove")).toEqual([]);
  expect((await uploads(ref)).map((f) => [f.path, f.size])).toEqual([["originals/a.png", existing.size]]);
  q.dispose();
});

it("retries a lost staging response with the same create ID and attaches the upload", async () => {
  const ref = await gallery();
  const rec = recorder();
  let dropped = false;
  const q = new UploadQueue(
    losing(rec, () => !dropped && (dropped = true), true),
    { ref, path },
  );
  const [up] = q.add([png("new.png", 2)]);
  await until(q, (x) => x.ready);
  const files = await q.commit();
  expect(rec.commits[0]![0]).toMatchObject({ create_id: up!.id, unattached: true });
  // The lost create is sent again as is, then attached by a new operation.
  expect(rec.commitRequests[1]).toEqual(rec.commitRequests[0]);
  expect(rec.commits.at(-1)).toMatchObject([{ op: "attach", path: "originals/new.png" }]);
  expect(rec.commitRequests.at(-1)!.operation_id).not.toBe(rec.commitRequests[0]!.operation_id);
  expect(files).toMatchObject([{ path: "originals/new.png" }]);
  expect(files[0]!.unattached).toBeUndefined();
  q.dispose();
});

it("commits only the uploaded head of the queue with head", async () => {
  const ref = await gallery();
  const q = new UploadQueue(media(), { ref, path });
  const [a, bad, c] = q.add([png("a.png", 1), gif(), png("c.png", 3)]);
  await until(q, (x) => x.items.every((i) => i.status !== "queued" && i.status !== "uploading"));
  const files = await q.commit(undefined, { head: true });
  expect(files.map((f) => f.path)).toEqual(["originals/a.png"]);
  const status = (id: string) => q.getSnapshot().items.find((i) => i.id === id)!.status;
  expect([status(a!.id), status(bad!.id), status(c!.id)]).toEqual(["committed", "failed", "uploaded"]);
  q.dispose();
});

it("processes on upload: stages unattached, polls until processed, attaches in queue order and discards a removed one", async () => {
  const ref = await gallery();
  const rec = recorder();
  // The worker is held on this item: the uploads stay processing until released.
  await h.faults([{ item: ref.id, fault: "hold" }]);
  const q = new UploadQueue(media(rec, true), { ref, path, pollInterval: 50 });
  let d;
  let b;
  try {
    [, b, d] = q.add([png("a.png", 1), png("b.png", 2), png("d.png", 3)]);
    await until(q, (x) => x.items.every((i) => i.unattached && i.processing && !i.processed));
  } finally {
    await h.clearFaults(ref.id);
  }
  await until(q, (x) => x.items.every((i) => i.processed));
  expect(rec.commits.flat().map((op) => [op.op, op.unattached])).toEqual([["put", true], ["put", true], ["put", true]]);
  q.remove(d!.id);
  await vi.waitFor(() => expect(rec.commits).toHaveLength(4), wait);
  expect(rec.commits[3]).toEqual([{ op: "remove", path: "originals/d.png" }]);
  q.move(b!.id, 0);
  const files = await q.commit();
  expect(rec.commits[4]!.map((op) => [op.op, op.path])).toEqual([["attach", "originals/b.png"], ["attach", "originals/a.png"]]);
  expect(files.map((f) => [f.path, !!f.unattached])).toEqual([["originals/b.png", false], ["originals/a.png", false]]);
  q.dispose();
});

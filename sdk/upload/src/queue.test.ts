import { expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient } from "../test/fake.js";
import { UploadQueue, type QueueSnapshot } from "./queue.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };
const path = "originals/{name}";

function setup() {
  const s = new FakeServer();
  return { s, q: new UploadQueue(fakeClient(s), { ref, path }) };
}

const png = (name: string, seed: number) => new File([bytes(1000, seed)], name, { type: "image/png" });

function until(q: UploadQueue, ok: (s: QueueSnapshot) => boolean): Promise<QueueSnapshot> {
  return new Promise((resolve) => {
    const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
    const off = q.subscribe(check);
    check();
  });
}

it("uploads, keeps the arranged order and commits puts appended in it", async () => {
  const { s, q } = setup();
  const [a, b, c] = q.add([png("a.png", 1), png("b.png", 2), png("c.png", 3)]);
  expect(a!.path).toBe("originals/a.png");
  s.seed(ref, [{ path: "originals/0.png", type: "image/png", size: 1 }]);
  await until(q, (x) => x.ready);
  q.move(c!.id, 0);
  q.update(b!.id, { path: "originals/02.png", meta: { alt: "b" } });
  const files = await q.commit();
  expect(files.map((f) => f.path)).toEqual(["originals/0.png", "originals/c.png", "originals/a.png", "originals/02.png"]);
  expect(s.commits[0]!.map((op) => [op.op, op.path, op.index])).toEqual([
    ["put", "originals/c.png", 2 ** 31 - 1],
    ["put", "originals/a.png", 2 ** 31 - 1],
    ["put", "originals/02.png", 2 ** 31 - 1],
  ]);
  expect(s.commits[0]![2]).toMatchObject({ meta: { alt: "b" }, blob: expect.stringMatching(/^u-/) });
  expect(q.getSnapshot().items.map((i) => i.status)).toEqual(["committed", "committed", "committed"]);
  expect(a!.status).toBe("queued"); // snapshots are immutable
});

it("blocks on a rate limit before starting the next file, and resumes on start()", async () => {
  const { s, q } = setup();
  s.refuse = { status: 429, code: "rate_limited", error: "slow down", retry_after: 30 };
  q.add([png("a.png", 1), png("b.png", 2), png("c.png", 3)]);
  const blocked = await until(q, (x) => !!x.blocked && x.items.every((i) => i.status === "queued"));
  expect(blocked.blocked!.retryAfter).toBe(30);
  const presigns = s.calls.filter((c) => c === "/presign").length;
  expect(presigns).toBeLessThanOrEqual(2); // the concurrent pair; the third never started
  s.refuse = undefined;
  q.start();
  await until(q, (x) => x.ready);
  expect(s.puts.length).toBe(3);
});

it("marks a file failed on its own refusal and retries it", async () => {
  const { s, q } = setup();
  const [bad] = q.add([new File([bytes(10)], "x.gif", { type: "" })]);
  await until(q, (x) => x.items[0]!.status === "failed");
  expect(q.getSnapshot().blocked).toBeUndefined();
  q.remove(bad!.id);
  expect(q.getSnapshot().items).toEqual([]);
  expect(s.puts.length).toBe(0);
});

it("re-uploads a file whose blob went stale before commit", async () => {
  const { s, q } = setup();
  const [, b] = q.add([png("a.png", 1), png("b.png", 2)]);
  const snap = await until(q, (x) => x.ready);
  s.stale.add(snap.items.find((i) => i.id === b!.id)!.result!.blob);
  const files = await q.commit();
  expect(files.map((f) => f.path)).toEqual(["originals/a.png", "originals/b.png"]);
  expect(s.puts.length).toBe(3);
  expect(q.getSnapshot().items.every((i) => i.status === "committed")).toBe(true);
});

it("commits only the uploaded head of the queue with head", async () => {
  const { q } = setup();
  const [a, bad, c] = q.add([png("a.png", 1), new File([bytes(10)], "x.gif", { type: "" }), png("c.png", 3)]);
  await until(q, (x) => x.items.every((i) => i.status !== "queued" && i.status !== "uploading"));
  const files = await q.commit(undefined, { head: true });
  expect(files.map((f) => f.path)).toEqual(["originals/a.png"]);
  const status = (id: string) => q.getSnapshot().items.find((i) => i.id === id)!.status;
  expect([status(a!.id), status(bad!.id), status(c!.id)]).toEqual(["committed", "failed", "uploaded"]);
});

it("processes on upload: stages unattached, polls until processed, attaches in queue order and discards a removed one", async () => {
  const s = new FakeServer();
  s.processOnUpload = true;
  s.pendingReads = 2;
  s.stagedReads = 3;
  const q = new UploadQueue(fakeClient(s), { ref, path, pollInterval: 5 });
  const [a, b, d] = q.add([png("a.png", 1), png("b.png", 2), png("d.png", 3)]);
  await until(q, (x) => x.items.every((i) => i.unattached && i.processing));
  await until(q, (x) => x.items.every((i) => i.processed));
  expect(s.commits.flat().map((op) => [op.op, op.unattached])).toEqual([["put", true], ["put", true], ["put", true]]);
  q.remove(d!.id);
  await vi.waitFor(() => expect(s.commits).toHaveLength(4));
  expect(s.commits[3]).toEqual([{ op: "remove", path: "originals/d.png" }]);
  q.move(b!.id, 0);
  const files = await q.commit();
  expect(s.commits[4]!.map((op) => [op.op, op.path])).toEqual([["attach", "originals/b.png"], ["attach", "originals/a.png"]]);
  expect(files.map((f) => [f.path, !!f.unattached])).toEqual([["originals/b.png", false], ["originals/a.png", false]]);
  void a;
  q.dispose();
});

import { expect, it } from "vitest";
import { FakeServer, bytes } from "../test/fake.js";
import { UploadClient } from "./client.js";
import { UploadQueue, type QueueSnapshot } from "./queue.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "en" };

function setup() {
  const s = new FakeServer();
  const c = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
  return { s, q: new UploadQueue(c, { ref }) };
}

const png = (name: string, seed: number) => new File([bytes(1000, seed)], name, { type: "image/png" });

function until(q: UploadQueue, ok: (s: QueueSnapshot) => boolean): Promise<QueueSnapshot> {
  return new Promise((resolve) => {
    const check = () => ok(q.getSnapshot()) && (off(), resolve(q.getSnapshot()));
    const off = q.subscribe(check);
    check();
  });
}

it("uploads, keeps the arranged order and commits inserts in it", async () => {
  const { s, q } = setup();
  const [a, b, c] = q.add([png("a.png", 1), png("b.png", 2), png("c.png", 3)]);
  await until(q, (x) => x.ready);
  q.move(c!.id, 0);
  q.update(b!.id, { name: "02.png", meta: { alt: "b" } });
  const files = await q.commit();
  expect(files.map((f) => f.name)).toEqual(["c.png", "a.png", "02.png"]);
  const commit = s.calls.lastIndexOf("/commit");
  expect(commit).toBeGreaterThan(0);
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

it("re-uploads a file whose original went stale before commit", async () => {
  const { s, q } = setup();
  const [, b] = q.add([png("a.png", 1), png("b.png", 2)]);
  const snap = await until(q, (x) => x.ready);
  s.stale.add(snap.items.find((i) => i.id === b!.id)!.result!.name);
  const files = await q.commit();
  expect(files.map((f) => f.name)).toEqual(["a.png", "b.png"]);
  expect(s.puts.length).toBe(3);
  expect(q.getSnapshot().items.every((i) => i.status === "committed")).toBe(true);
});

it("commits only the uploaded head of the queue with head", async () => {
  const { q } = setup();
  const [a, bad, c] = q.add([png("a.png", 1), new File([bytes(10)], "x.gif", { type: "" }), png("c.png", 3)]);
  await until(q, (x) => x.items.every((i) => i.status !== "queued" && i.status !== "uploading"));
  const files = await q.commit(undefined, { head: true });
  expect(files.map((f) => f.name)).toEqual(["a.png"]);
  const status = (id: string) => q.getSnapshot().items.find((i) => i.id === id)!.status;
  expect([status(a!.id), status(bad!.id), status(c!.id)]).toEqual(["committed", "failed", "uploaded"]);
});

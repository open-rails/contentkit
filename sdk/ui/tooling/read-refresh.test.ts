import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { registerHooks } from "node:module";
import test, { mock } from "node:test";
import { fileURLToPath } from "node:url";
import type { ContentKitClient } from "../src/client/client.ts";
import type { ReadResult } from "../src/client/generated/wire.ts";

// The sources import each other as .js (bundler resolution) and the hash
// worker through Vite; run them from .ts with the worker stubbed.
registerHooks({
  resolve(specifier, context, next) {
    if (specifier.endsWith("?worker&inline")) return { url: "stub:worker", shortCircuit: true };
    if (specifier.startsWith(".") && specifier.endsWith(".js") && context.parentURL) {
      const ts = new URL(specifier.replace(/\.js$/, ".ts"), context.parentURL);
      if (existsSync(fileURLToPath(ts))) return next(ts.href, context);
    }
    return next(specifier, context);
  },
  load(url, context, next) {
    if (url === "stub:worker") return { format: "module", source: "export default class {}", shortCircuit: true };
    return next(url, context);
  },
});
const { ContentKitError } = await import("../src/client/errors.ts");
const { ResourceStore } = await import("../src/react/resources.ts");
const { ReadStore } = await import("../src/react/store.ts");

const settle = () => new Promise((resolve) => setImmediate(resolve));
const outage = () => new ContentKitError("server_unavailable", "down", { status: 503 });

test("a retried failed resource keeps its error until the retry succeeds", async () => {
  const store = new ResourceStore({ subscribe: () => () => {} } as unknown as ContentKitClient);
  let answer: () => Promise<string> = () => Promise.reject(outage());
  store.subscribe("plan", { type: "reader-plan" }, () => {});
  store.bind("plan", () => answer());
  store.load("plan");
  await settle();
  const failed = store.snapshot("plan").error;
  assert.equal(failed?.code, "server_unavailable");

  let release!: (plan: string) => void;
  answer = () => new Promise((resolve) => (release = resolve));
  store.load("plan", true);
  assert.equal(store.snapshot("plan").loading, true);
  assert.equal(store.snapshot("plan").error, failed, "the error must not blink while the retry is in flight");

  release("plan");
  await settle();
  assert.equal(store.snapshot("plan").data, "plan");
  assert.equal(store.snapshot("plan").error, undefined);
});

test("a transient refresh failure keeps the unexpired read and retries; a refusal drops it", async (t) => {
  mock.timers.enable({ apis: ["setTimeout"], now: Date.now() });
  t.after(() => mock.timers.reset());
  const grant = (url: string): ReadResult => ({ access: "full", expires: Math.floor(Date.now() / 1000) + 90, total: 1, offset: 0, limit: 1, files: [{ path: "p/1.jpg", url }] } as ReadResult);
  const answers: (() => Promise<ReadResult>)[] = [async () => grant("first"), () => Promise.reject(outage()), async () => grant("second")];
  let calls = 0;
  const client = { subscribe: () => () => {}, media: { read: () => answers[calls++]!() } } as unknown as ContentKitClient;
  const store = new ReadStore(client);
  const ref = { kind: "gallery", id: "g1" };
  store.subscribe("read", ref, {}, () => {}, { refresh: true });
  store.load("read");
  await settle();
  const first = store.snapshot("read").read;
  assert.equal(first?.files[0]?.url, "first");

  // The scheduled pre-expiry refresh hits an outage.
  mock.timers.tick(30_000);
  await settle();
  assert.equal(calls, 2);
  assert.equal(store.snapshot("read").error?.code, "server_unavailable");
  assert.equal(store.snapshot("read").read, first, "the still-valid grant must stay shown");

  // The kept read is retried before it expires.
  mock.timers.tick(30_000);
  await settle();
  assert.equal(calls, 3);
  assert.equal(store.snapshot("read").read?.files[0]?.url, "second");
  assert.equal(store.snapshot("read").error, undefined);

  answers.push(() => Promise.reject(new ContentKitError("forbidden", "no", { status: 403 })));
  store.load("read", "reload");
  await settle();
  assert.equal(store.snapshot("read").read, null);
  assert.equal(store.snapshot("read").error?.code, "forbidden");
});


test("an active aborted resource settles without leaking a departed account or retrying itself", async () => {
  const store = new ResourceStore({ subscribe: () => () => {} } as unknown as ContentKitClient);
  let releaseAlice!: (value: { favorited: boolean }) => void;
  const leaveAlice = store.subscribe("alice-favorite", { type: "favorite", readScope: "alice" }, () => {});
  store.bind("alice-favorite", () => new Promise((resolve) => { releaseAlice = resolve; }));
  store.load("alice-favorite");
  leaveAlice();
  await settle();

  let calls = 0;
  const aborted = new ContentKitError("aborted", "The account changed.");
  store.subscribe("bob-favorite", { type: "favorite", readScope: "bob" }, () => {});
  const unbind = store.bind("bob-favorite", async () => { calls++; throw aborted; });
  store.load("bob-favorite");
  await settle();
  assert.equal(store.snapshot("bob-favorite").loading, false);
  assert.equal(store.snapshot("bob-favorite").loaded, true);
  assert.equal(store.snapshot("bob-favorite").error, aborted);
  assert.equal(store.snapshot("bob-favorite").data, undefined);
  assert.equal(calls, 1, "an abort must not start an automatic retry loop");

  releaseAlice({ favorited: true });
  await settle();
  assert.equal(store.snapshot("bob-favorite").data, undefined, "late Alice data cannot fill Bob's slot");
  unbind();
  store.bind("bob-favorite", async () => ({ favorited: false }));
  store.load("bob-favorite", true);
  assert.equal(store.snapshot("bob-favorite").error, aborted, "explicit retry retains the failure until it settles");
  await settle();
  assert.deepEqual(store.snapshot("bob-favorite").data, { favorited: false });
  assert.equal(store.snapshot("bob-favorite").error, undefined);
  assert.equal(store.snapshot("bob-favorite").loading, false);
});

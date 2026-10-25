import assert from "node:assert/strict";
import { createContentKitClient } from "../../sdk/ui/dist/client.js";

// This deliberately controls the injected boundary. Real AuthKit HTTP and
// account authorization are exercised by the backend and host browser cases.
const item = {
  id: "receipt", recipient: { audience: "user", id: "alice" },
  origin: { system: "contentkit", scope: "app", id: "comment.reply:B" },
  kind: "comment.reply", data: {}, created_at: "2026-10-24T00:00:00Z",
  updated_at: "2026-10-24T00:00:00Z", revision: 1,
  read_at: null, dismissed_at: null, resolved_at: null,
};
const listeners = new Set();
const calls = [];
let pendingRead;
const transport = {
  async list(options) { calls.push(["list", options]); return { items: [item], next_cursor: "" }; },
  async setRead(id, read, signal) {
    calls.push(["read", id, read, signal]);
    if (pendingRead) await pendingRead;
    return { ...item, read_at: read ? "2026-10-24T00:01:00Z" : null };
  },
  async setDismissed(id, dismissed, signal) {
    calls.push(["dismiss", id, dismissed, signal]);
    return { ...item, dismissed_at: dismissed ? "2026-10-24T00:02:00Z" : null };
  },
  subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); },
  refresh() { for (const listener of listeners) listener(); },
};
const client = createContentKitClient({
  baseUrl: "https://unused.invalid/content", notifications: transport,
  fetch() { throw new Error("injected inbox unexpectedly used ContentKit HTTP"); },
});
const first = [], second = [];
const stopFirst = client.subscribe((...change) => first.push(change));
const stopSecond = client.subscribe((...change) => second.push(change));
client.setReadScope("alice");
assert.equal(listeners.size, 1);
const signal = new AbortController().signal;
const options = { unread: true, limit: 7, cursor: "next", signal };
assert.deepEqual((await client.notifications.list(options)).items, [item]);
assert.deepEqual(calls.shift(), ["list", options]);
await client.notifications.setRead("receipt", true, signal);
await client.notifications.setDismissed("receipt", true, signal);
assert.deepEqual(calls.splice(0), [["read", "receipt", true, signal], ["dismiss", "receipt", true, signal]]);
assert.deepEqual(first.map(([change]) => change.type), ["notifications.updated", "notifications.updated"]);
client.notifications.refresh();
assert.deepEqual(first.at(-1), [{ type: "notifications.changed" }, "alice"]);
assert.deepEqual(second.at(-1), first.at(-1));
// A receipt request started by Alice must be marked stale if a host switches
// accounts before it completes; null tells caches to refetch, never apply it
// as Bob's receipt.
let complete;
pendingRead = new Promise((resolve) => { complete = resolve; });
const request = client.notifications.setRead("receipt", false);
client.setReadScope("bob");
complete();
await request;
assert.equal(first.at(-1)[1], null);
stopFirst();
assert.equal(listeners.size, 1);
stopSecond(); stopSecond();
assert.equal(listeners.size, 0);
console.log("injected notification transport: delegation, refresh, account scope and cleanup passed");

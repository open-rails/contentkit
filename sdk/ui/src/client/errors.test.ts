import { expect, it } from "vitest";
import { readContentKitError as fromResponse } from "./errors.js";

it("maps error replies, Retry-After and non-JSON bodies", async () => {
  const rate = await fromResponse(
    new Response(JSON.stringify({ error: "slow down", code: "rate_limited", retry_after: 7 }), { status: 429 }),
  );
  expect([rate.code, rate.status, rate.retryAfter, rate.isLimit, rate.blocksQueue, rate.transient]).toEqual([
    "rate_limited", 429, 7, true, true, false,
  ]);
  const header = await fromResponse(new Response("<html>", { status: 429, headers: { "Retry-After": "3" } }));
  expect([header.code, header.retryAfter]).toEqual(["rate_limited", 3]);
  const quota = await fromResponse(new Response(JSON.stringify({ error: "full", code: "quota_exceeded" }), { status: 413 }));
  expect([quota.code, quota.isLimit]).toEqual(["quota_exceeded", true]);
  const proxy = await fromResponse(new Response("bad gateway", { status: 502 }));
  expect([proxy.code, proxy.transient]).toEqual(["internal_error", true]);
  const down = await fromResponse(new Response(JSON.stringify({ error: "storage down", code: "unavailable", retry_after: 5 }), { status: 503 }));
  expect([down.code, down.transient, down.retryAfter]).toEqual(["unavailable", true, 5]);
  const gone = await fromResponse(new Response(JSON.stringify({ error: "again", code: "not_uploaded", blobs: ["sha256-a"] }), { status: 409 }));
  expect([gone.code, gone.blobs]).toEqual(["not_uploaded", ["sha256-a"]]);
});

it("reads the content modules' ban and rate-limit fields and unknown codes", async () => {
  const until = "2026-11-01T00:00:00Z";
  const banned = await fromResponse(
    new Response(JSON.stringify({ error: "you can't comment here", code: "comment_banned", ban: { scope: "global", until } }), { status: 403 }),
  );
  expect([banned.code, banned.ban, banned.blocksQueue]).toEqual(["comment_banned", { scope: "global", until }, false]);
  const limited = await fromResponse(
    new Response(JSON.stringify({ error: "slow down", code: "rate_limited", action: "comment", retry_after: 30 }), { status: 429 }),
  );
  expect([limited.action, limited.retryAfter, limited.isLimit]).toEqual(["comment", 30, true]);
  const future = await fromResponse(new Response(JSON.stringify({ error: "nope", code: "some_new_code" }), { status: 410 }));
  expect([future.code, future.message]).toEqual(["gone", "nope"]);
});

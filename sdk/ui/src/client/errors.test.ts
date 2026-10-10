import { expect, it } from "vitest";
import { fromResponse } from "./errors.js";

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

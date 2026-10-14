import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createTranslator, resolveMessages } from "../i18n/messages.js";
import { en } from "../locales/en.js";
import { ja } from "../locales/ja.js";
import { readContentKitError as fromResponse, toContentKitError, transportFailure, type ContentKitError } from "./errors.js";
import { Http } from "./http.js";

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
  expect([proxy.code, proxy.transient]).toEqual(["server_unavailable", true]);
  const crash = await fromResponse(new Response("oops", { status: 500 }));
  expect([crash.code, crash.transient]).toEqual(["internal_error", true]);
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
  expect([future.code, future.message, future.unknownCode]).toEqual(["gone", "nope", "some_new_code"]);
});

describe("failures without a ContentKit answer", () => {
  const say = createTranslator(resolveMessages()).error;
  // A real server: each path answers with its status and body; /slow never answers.
  const replies: Record<string, [number, string]> = {
    "/gateway-503": [503, "<html>Service Unavailable</html>"],
    "/cloudflare-524": [524, ""],
    "/coded-503": [503, JSON.stringify({ error: "media storage down", code: "unavailable" })],
    "/unknown-409": [409, JSON.stringify({ error: "that handle is reserved", code: "handle_reserved" })],
    "/unknown-503": [503, JSON.stringify({ error: "the signal plane is draining", code: "draining" })],
  };
  let server: Server;
  let base = "";
  beforeAll(async () => {
    server = createServer((req, res) => {
      const reply = replies[req.url ?? ""];
      if (!reply) return;
      res.writeHead(reply[0], { "Content-Type": reply[1].startsWith("{") ? "application/json" : "text/html" }).end(reply[1]);
    });
    await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
    base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });
  afterAll(() => {
    server.closeAllConnections();
    server.close();
  });
  const get = (path: string, signal?: AbortSignal) =>
    new Http({ baseUrl: base }).request(base + path, { signal }).then(
      () => expect.unreachable(),
      (e: ContentKitError) => e,
    );

  it("names a server no request reached", async () => {
    const closed = createServer();
    await new Promise<void>((done) => closed.listen(0, "127.0.0.1", done));
    const port = (closed.address() as AddressInfo).port;
    await new Promise((done) => closed.close(done));
    const err = await new Http({ baseUrl: "" }).request<never>(`http://127.0.0.1:${port}/v1/x`).catch((e: ContentKitError) => e);
    expect([err.code, err.status, err.transient]).toEqual(["network", 0, true]);
    expect(say(err)).toBe("Couldn't reach the server. Check your connection and try again.");
    for (const message of ["Failed to fetch", "NetworkError when attempting to fetch resource.", "Load failed"])
      expect(say(new TypeError(message)), message).toBe(en.errors.network);
    const bug = toContentKitError(new TypeError("x.map is not a function"));
    expect([bug.code, say(bug)]).toEqual(["internal_error", en.errors.generic]);
  });

  it("names a server that doesn't answer: a gateway without an API body, or a timeout", async () => {
    for (const path of ["/gateway-503", "/cloudflare-524"]) {
      const err = await get(path);
      expect([err.code, err.transient, say(err)], path).toEqual(["server_unavailable", true, "The server isn't responding right now. Please try again in a moment."]);
    }
    const slow = await get("/slow", AbortSignal.timeout(50));
    expect([slow.code, say(slow)]).toEqual(["server_unavailable", en.errors.server_unavailable]);
    expect(transportFailure(new DOMException("timed out", "TimeoutError"))).toBe("server_unavailable");
    const ctl = new AbortController();
    const canceled = get("/slow", ctl.signal);
    ctl.abort();
    expect((await canceled).code).toBe("aborted");
    expect(createTranslator(resolveMessages(ja)).error(await get("/gateway-503"))).toBe(ja.errors!.server_unavailable);
  });

  it("keeps a coded 503 as the server sent it", async () => {
    const err = await get("/coded-503");
    expect([err.code, err.unknownCode, say(err)]).toEqual(["unavailable", undefined, en.errors.unavailable]);
    expect(transportFailure(err)).toBeUndefined();
  });

  it("shows the server's own message for a code it doesn't know", async () => {
    const reserved = await get("/unknown-409");
    expect([reserved.code, reserved.unknownCode, say(reserved)]).toEqual(["conflict", "handle_reserved", "That handle is reserved."]);
    const draining = await get("/unknown-503");
    expect([draining.code, draining.transient, say(draining)]).toEqual(["unavailable", true, "The signal plane is draining."]);
  });
});

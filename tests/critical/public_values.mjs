// Literal public-contract vectors against the shipped framework-free bundle.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import * as sdk from "../../sdk/ui/dist/client.js";

const checks = new Map();
const check = (name, run) => checks.set(name, run);
const uuid = "019c6e27-e55b-73d1-87d8-4e01f1f75043";
const imageName = "i-af985306-d261-4499-8c67-eccdcde5612e";
const ref = { kind: "gallery", id: uuid, version: "v1" };

check("imageRef", () => assert.equal(sdk.imageRef(imageName), `contentkit:${imageName}`));
check("imageRefs", () => {
  const input = `before contentkit:${imageName}, after contentkit:i-invalid`;
  assert.deepEqual([...input.matchAll(sdk.imageRefs())].map(match => match[1]), [imageName]);
  assert.deepEqual([...input.matchAll(sdk.imageRefs())].map(match => match[1]), [imageName]);
});
check("allowsEntityScope", () => {
  assert.equal(sdk.allowsEntityScope(undefined, "content"), true);
  assert.equal(sdk.allowsEntityScope(undefined, "version"), false);
  assert.equal(sdk.allowsEntityScope({ version_level: true }, "version"), true);
  assert.equal(sdk.allowsEntityScope({ membership_scopes: ["content"], version_level: true }, "version"), false);
  assert.equal(sdk.allowsEntityScope({ membership_scopes: ["content", "version"] }, "version"), true);
});
check("isContentKitError", () => {
  assert.equal(sdk.isContentKitError(new sdk.ContentKitError("unauthorized", "Sign in")), true);
  assert.equal(sdk.isContentKitError({ name: "ContentKitError", code: "unauthorized" }), false);
  assert.equal(sdk.isContentKitError(new Error("other")), false);
});
check("toContentKitError", () => {
  const known = new sdk.ContentKitError("too_large", "Too large", { status: 413 });
  assert.equal(sdk.toContentKitError(known), known);
  const cause = new TypeError("fetch failed");
  const wrapped = sdk.toContentKitError(cause);
  assert.equal(wrapped.code, "network"); assert.equal(wrapped.cause, cause);
  assert.equal(sdk.toContentKitError("opaque failure").message, "opaque failure");
});
check("transportFailure", () => {
  assert.equal(sdk.transportFailure(new TypeError("Network connection failed")), "network");
  assert.equal(sdk.transportFailure(new TypeError("Invalid application argument")), undefined);
  assert.equal(sdk.transportFailure({ name: "TimeoutError" }), "server_unavailable");
  assert.equal(sdk.transportFailure({ code: "network_error" }), "network");
  assert.equal(sdk.transportFailure({ status: 502 }), "server_unavailable");
  assert.equal(sdk.transportFailure({ code: "forbidden", status: 403 }), undefined);
  assert.equal(sdk.transportFailure(new DOMException("Canceled", "AbortError")), undefined);
});
check("readContentKitError", async () => {
  const body = { code: "quota_exceeded", error: "Quota is full", retry_after: 7, details: { max_bytes: 12 }, blobs: ["b1"], ban: { scope: "owner:x", reason: "policy" }, action: "upload", reason: "release_window", available_at: "2030-01-01T00:00:00Z" };
  const error = await sdk.readContentKitError(new Response(JSON.stringify(body), { status: 429, headers: { "Retry-After": "11" } }));
  assert.equal(error.code, body.code); assert.equal(error.message, body.error); assert.equal(error.status, 429); assert.equal(error.retryAfter, 7);
  assert.deepEqual(error.details, body.details); assert.deepEqual(error.blobs, body.blobs); assert.deepEqual(error.ban, body.ban);
  assert.equal(error.action, "upload"); assert.equal(error.reason, "release_window"); assert.equal(error.availableAt, body.available_at);
  const newer = await sdk.readContentKitError(new Response(JSON.stringify({ code: "future_server_code", error: "Specific future refusal" }), { status: 409 }));
  assert.equal(newer.code, "conflict"); assert.equal(newer.unknownCode, "future_server_code"); assert.equal(newer.message, "Specific future refusal");
  const proxy = await sdk.readContentKitError(new Response("<html>gateway unavailable</html>", { status: 503, headers: { "Retry-After": "5" } }));
  assert.equal(proxy.code, "server_unavailable"); assert.equal(proxy.retryAfter, 5); assert.equal(proxy.transient, true);
  for (const [status, code] of [[401,"unauthorized"],[403,"forbidden"],[404,"not_found"],[409,"conflict"],[410,"gone"],[413,"too_large"],[415,"type_not_allowed"],[429,"rate_limited"],[501,"not_configured"],[500,"internal_error"],[400,"invalid_request"]]) {
    const fallback = await sdk.readContentKitError(new Response(JSON.stringify({ error: "Specific refusal" }), { status }));
    assert.equal(fallback.status, status); assert.equal(fallback.code, code); assert.equal(fallback.message, "Specific refusal");
  }
});
check("failureError", () => {
  const error = sdk.failureError({ code: "too_large", message: "Stored failure", details: { max_bytes: 2 } });
  assert.equal(error.code, "too_large"); assert.equal(error.message, "Stored failure"); assert.deepEqual(error.details, { max_bytes: 2 });
  assert.equal(sdk.failureError({ code: "unknown_processing_failure", message: "Lost worker" }).code, "internal_error");
});
check("ContentKitError.refusal", () => {
  assert.equal(new sdk.ContentKitError("forbidden", "No", { status: 403 }).refusal, true);
  assert.equal(new sdk.ContentKitError("decode", "Image cannot decode").refusal, true);
  assert.equal(new sdk.ContentKitError("storage", "S3 expired", { status: 403 }).refusal, false);
  assert.equal(new sdk.ContentKitError("internal_error", "Server", { status: 500 }).refusal, false);
});
check("ContentKitError.isLimit", () => {
  for (const code of ["rate_limited", "quota_exceeded"]) assert.equal(new sdk.ContentKitError(code, "Limit").isLimit, true);
  assert.equal(new sdk.ContentKitError("too_large", "One file").isLimit, false);
});
check("ContentKitError.isCeiling", () => {
  assert.equal(new sdk.ContentKitError("too_many_files", "Remove a file", { status: 409 }).isCeiling, true);
  assert.equal(new sdk.ContentKitError("quota_exceeded", "Storage limit").isCeiling, false);
});
check("ContentKitError.blocksQueue", () => {
  for (const code of ["quota_exceeded", "rate_limited", "unauthorized", "forbidden"]) assert.equal(new sdk.ContentKitError(code, "Stop").blocksQueue, true);
  assert.equal(new sdk.ContentKitError("too_large", "Only this file").blocksQueue, false);
});
check("ContentKitError.transient", () => {
  for (const code of ["network", "server_unavailable", "unavailable", "internal_error"]) assert.equal(new sdk.ContentKitError(code, "Retry").transient, true);
  for (const status of [403, 408, 429, 503]) assert.equal(new sdk.ContentKitError("storage", "Provider", { status }).transient, true);
  assert.equal(new sdk.ContentKitError("storage", "Bad object", { status: 400 }).transient, false);
  assert.equal(new sdk.ContentKitError("aborted", "Canceled").transient, false);
});
check("sha256Hex", async () => {
  const progress = [];
  assert.equal(await sdk.sha256Hex(new Blob(["abc"]), { onProgress: value => progress.push(value) }), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  assert.equal(progress.at(-1), 3);
  const bytes = Buffer.alloc(65 * 1024 * 1024, 97);
  assert.equal(await sdk.sha256Hex(new Blob([bytes])), createHash("sha256").update(bytes).digest("hex"));
  const abort = new AbortController(); abort.abort();
  await assert.rejects(() => sdk.sha256Hex(new Blob(["abc"]), { signal: abort.signal }), error => error.code === "aborted");
});
check("isContentId", () => {
  assert.equal(sdk.isContentId(uuid), true);
  for (const bad of [uuid.toUpperCase(), "af985306-d261-4499-8c67-eccdcde5612e", "00000000-0000-0000-0000-000000000000", "019c6e27-e55b-73d1-17d8-4e01f1f75043", "", ` ${uuid}`, `${uuid} `]) assert.equal(sdk.isContentId(bad), false);
});
check("itemKey", () => {
  assert.equal(sdk.itemKey(ref), `gallery/${uuid}/v1`);
  assert.equal(sdk.itemKey({ kind: "gallery", id: uuid }), `gallery/${uuid}`);
  assert.equal(sdk.itemKey({ kind: "article", id: "019c6e27-e55b-73d1-87d8-4e01f1f75044" }), "article/019c6e27-e55b-73d1-87d8-4e01f1f75044");
});
check("sameItem", () => {
  assert.equal(sdk.sameItem(ref, { ...ref }), true);
  assert.equal(sdk.sameItem(ref, { ...ref, version: "v2" }), false);
  assert.equal(sdk.sameItem(ref, { ...ref, kind: "article" }), false);
  assert.equal(sdk.sameItem(ref, { ...ref, id: "019c6e27-e55b-73d1-87d8-4e01f1f75044" }), false);
  assert.equal(sdk.sameItem({ kind: "gallery", id: uuid }, { kind: "gallery", id: uuid, version: "" }), true);
});
check("checkRef", () => {
  assert.doesNotThrow(() => sdk.checkRef(ref)); assert.doesNotThrow(() => sdk.checkRef(undefined));
  assert.throws(() => sdk.checkRef({ id: "../../another-item" }), error => error.code === "invalid_request");
});
const rules = [{ path: "pages/{name}", types: ["image/png"], max_bytes: 3, max: 2 }, { path: "pages/cover", types: ["image/png"], max_bytes: 3, max: 1 }];
check("uploadRules", () => {
  assert.equal(sdk.uploadRules({ uploads: rules }), rules);
  assert.deepEqual(sdk.uploadRules(null), []); assert.equal(sdk.uploadRules(null), sdk.uploadRules(undefined));
});
check("ruleFor", () => {
  assert.deepEqual(sdk.ruleFor(rules, "pages/a.png"), rules[0]); assert.deepEqual(sdk.ruleFor(rules, "pages/cover"), rules[1]);
  assert.equal(sdk.ruleFor(rules, "pages/nested/a.png"), undefined); assert.equal(sdk.ruleFor(rules, "other/a.png"), undefined);
});
check("filesFor", () => {
  const accepted = { path: "pages/a.png", upload: true };
  const read = { files: [accepted, { path: "pages/unattached.png", upload: true, unattached: true }, { path: "pages/output.webp", upload: false }, { path: "pages/cover", upload: true }] };
  assert.deepEqual(sdk.filesFor(read, rules[0], rules), [accepted]);
});
check("screenFiles", () => {
  const files = [new File(["ok"], "1.png", { type: "image/png" }), new File(["huge"], "2.png", { type: "image/png" }), new File(["x"], "3.txt", { type: "text/plain" }), new File(["ok"], "4.png", { type: "image/png" })];
  const screened = sdk.screenFiles(files, [rules[0]], () => 1);
  assert.equal(screened.accepted.length, 1); assert.equal(screened.accepted[0].file.name, "1.png"); assert.equal(screened.accepted[0].file.size, 2); assert.equal(screened.accepted[0].file.type, "image/png");
  assert.deepEqual(screened.refused.map(row => [row.error.code, row.error.status]), [["too_large", 413], ["type_not_allowed", 415], ["too_many_files", 409]]);
  assert.equal(screened.refused[0].error.details.max_bytes, 3);
});
check("uniqueName", () => {
  assert.equal(sdk.uniqueName("photo.jpg", ["photo.png", "photo-2.gif"]), "photo-3.jpg");
  assert.equal(sdk.uniqueName("new.png", ["photo.png"]), "new.png");
});
check("chunkOffsets", () => {
  assert.deepEqual(sdk.chunkOffsets(49, 51), [0, 50]); assert.deepEqual(sdk.chunkOffsets(50, 160, 101), [50, 100]);
  assert.deepEqual(sdk.chunkOffsets(-10, 10), [0]); assert.deepEqual(sdk.chunkOffsets(100, 90), []);
});
check("mergeReads", () => {
  const old = { expires: 100, offset: 0, limit: 1, files: [{ path: "a", url: "signed-a" }, { path: "removed", url: "retired" }] };
  const newer = { expires: 120, offset: 1, limit: 1, files: [{ path: "a", w: 42 }, { path: "b", url: "signed-b" }] };
  const snapshot = JSON.stringify([old, newer]);
  const merged = sdk.mergeReads([old, null, newer]);
  assert.deepEqual(merged.files, [{ path: "a", w: 42, url: "signed-a" }, { path: "b", url: "signed-b" }]);
  assert.equal(merged.expires, 100); assert.equal(merged.offset, 0); assert.equal(merged.limit, 2); assert.equal(JSON.stringify([old, newer]), snapshot);
  assert.equal(sdk.mergeReads([null, undefined]), null);
});
check("isProcessing", () => {
  assert.equal(sdk.isProcessing({ upload: true, staged: true }), true); assert.equal(sdk.isProcessing({ upload: true, pending: ["resize"] }), true);
  assert.equal(sdk.isProcessing({ upload: true, staged: true, failed: true }), false); assert.equal(sdk.isProcessing({ upload: false, pending: ["resize"] }), false);
});
check("processing", () => {
  assert.equal(sdk.processing({ state: "processing", files: [] }), true);
  assert.equal(sdk.processing({ state: "ready", files: [{ upload: true, staged: true }] }), true);
  assert.equal(sdk.processing({ state: "processing", full: true, files: [] }), false);
  assert.equal(sdk.processing({ status: { state: "full" }, files: [{ upload: true, staged: true }] }), false);
  assert.equal(sdk.processing(null), false);
});
check("statusKind", () => {
  assert.equal(sdk.statusKind(403), "access"); assert.equal(sdk.statusKind(410), "not_found"); assert.equal(sdk.statusKind(429), "rate_limited"); assert.equal(sdk.statusKind(503), "network");
});
check("expiryDelay", () => {
  assert.equal(sdk.expiryDelay(undefined, 100_000), null); assert.equal(sdk.expiryDelay(200, 100_000), 40_000);
  assert.equal(sdk.expiryDelay(160, 100_000), 30_000); assert.equal(sdk.expiryDelay(90, 100_000), 30_000);
});
check("refreshable", () => {
  assert.equal(sdk.refreshable({ kind: "access", status: 403 }), true); assert.equal(sdk.refreshable({ kind: "not_found", status: 404 }), true);
  assert.equal(sdk.refreshable({ kind: "rate_limited", status: 429 }), false); assert.equal(sdk.refreshable({ kind: "network", status: 0 }), false);
});
check("publicURL", () => {
  assert.equal(sdk.publicURL("https://media.invalid/", "app", "gallery", "parent", "cover.webp", "edition"), "https://media.invalid/v1/app/gallery/edition/public/cover.webp");
  assert.equal(sdk.publicURL("https://media.invalid", "accounts", "avatar", "owner", "small.webp"), "https://media.invalid/v1/accounts/avatar/owner/public/small.webp");
  assert.equal(sdk.publicURL("https://media.invalid", "ignored", "gallery", "parent", "cover.webp", "edition", "tenant/business/app/product/"), "https://media.invalid/v1/tenant/business/app/product/gallery/edition/public/cover.webp");
});
check("publicRenditions", () => {
  const renditions = [{ url: "wide-original-name.webp", w: 800 }, { url: "small-original-name.webp", w: 200 }];
  assert.deepEqual(sdk.publicRenditions({ renditions }), [renditions[1], renditions[0]]); assert.equal(renditions[0].w, 800); assert.deepEqual(sdk.publicRenditions(null), []);
});
check("isPending", () => {
  assert.equal(sdk.isPending({ status: "processing" }), true); assert.equal(sdk.isPending({ status: "processing", pages: [] }), false);
  assert.equal(sdk.isPending({ status: "processing", url: "ready" }), false); assert.equal(sdk.isPending(null), false);
});

const results = [];
for (const [name, run] of checks) {
  try { await run(); results.push({ name, passed: true }); }
  catch (error) { results.push({ name, passed: false, error: error?.stack ?? String(error) }); }
}
console.log(JSON.stringify(results));

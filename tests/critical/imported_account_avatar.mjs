// The shipped client writes and reads an imported account's avatar, whose id
// is a fixed UUIDv5, as a site's account page does.
import assert from "node:assert/strict";
import { createContentKitClient } from "../../sdk/ui/dist/client.js";

const fixture = JSON.parse(process.env.CONTENTKIT_AVATAR_FIXTURE);
const ref = { kind: "user", id: fixture.account };
const connect = (hold) => {
  const requests = [];
  const client = createContentKitClient({
    baseUrl: fixture.base,
    token: () => fixture.token,
    fetch: async (input, init) => {
      const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
      requests.push(path);
      if (hold && path.endsWith("/kinds")) await hold;
      return fetch(input, init);
    },
  });
  return { client, requests };
};
const kindsReads = (requests) => requests.filter((path) => path.endsWith("/kinds")).length;

// A client that has not read the content model reads it once, before any
// media request: a content kind still refuses the UUIDv5.
const fresh = connect();
await assert.rejects(fresh.client.media.read({ kind: "gallery", id: ref.id }), (e) => e.code === "invalid_request" && e.message.includes("UUIDv7"));
assert.deepEqual(fresh.requests.map((path) => path.split("/").pop()), ["kinds"]);
await fresh.client.media.read(ref);
await fresh.client.media.read(ref);
assert.equal(kindsReads(fresh.requests), 1, `account kind lookups: ${fresh.requests}`);

// An upload is the first request of another such client.
const uploader = connect();
const avatar = () => new File([Buffer.from(fixture.image, "base64")], "avatar.png", { type: "image/png" });
await uploader.client.media.put(avatar(), { ref, path: "avatar", wait: true, timeout: 60000 });
assert.equal(kindsReads(uploader.requests), 1, `account kind lookups: ${uploader.requests}`);

// A cancel ends a request that waits for the content model.
let release;
const held = connect(new Promise((resolve) => (release = resolve)));
const cancel = new AbortController();
const waiting = held.client.media.read(ref, { signal: cancel.signal });
cancel.abort();
await assert.rejects(waiting, (e) => e.code === "aborted");
release();

// A client that read the content model (useKinds) needs no other lookup.
const { client, requests } = connect();
const model = await client.kinds();
assert.equal(model.avatars?.kind, ref.kind);
await client.media.put(avatar(), { ref, path: "avatar", wait: true, timeout: 60000 });
const read = await client.media.read(ref);
assert.equal(kindsReads(requests), 1, `account kind lookups: ${requests}`);
const rendition = read.public?.find((p) => p.preset === "avatar")?.renditions[0];
assert.ok(rendition?.url.includes(`/account/user/${ref.id}/public/`), JSON.stringify(read.public));
const served = await fetch(rendition.url);
assert.equal(served.status, 200);
assert.equal(served.headers.get("content-type"), "image/webp");

// Content ids stay UUIDv7 and account ids stay canonical, refused before any request.
const sent = requests.length;
await assert.rejects(client.media.read({ kind: "gallery", id: ref.id }), (e) => e.code === "invalid_request" && e.message.includes("UUIDv7"));
await assert.rejects(client.media.read({ kind: ref.kind, id: "00000000-0000-0000-0000-000000000000" }), (e) => e.code === "invalid_request" && e.message.includes("account id"));
assert.equal(requests.length, sent);

console.log(JSON.stringify({ status: "passed" }));

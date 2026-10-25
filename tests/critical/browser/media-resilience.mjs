import assert from "node:assert/strict";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";

const fixture = JSON.parse(process.env.CONTENTKIT_BROWSER_FIXTURE);
const browser = await chromium.launch({headless: true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM ? {executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM} : {})});
async function bounded(promise, label, timeout = 30_000) {
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(Error(label)), timeout); })]);
  } finally { clearTimeout(timer); }
}
try {
  const page = await browser.newPage();
  page.on("pageerror", error => console.error("browser error:", error.message));
  page.setDefaultTimeout(30_000);
  await page.goto(fixture.url);
  await page.waitForFunction(() => !!window.mediaFactory);
  await page.evaluate(async fixture => {
    window.fixture = fixture;
    window.client = mediaFactory.createContentKitClient({baseUrl: "/content", token: () => fixture.token, media: {concurrency: 1, retries: 0}});
    window.image = await (await fetch("/fixture.png")).blob();
    window.queue = new mediaFactory.ItemUploadQueue(client.media, {ref: fixture.ref, path: "pages/{name}", autoStart: false, concurrency: 1});
    window.notifications = 0;
    window.unsubscribe = queue.subscribe(() => notifications++);
    const items = queue.add(["one.png", "two.png", "removed.png"].map(name => new File([image], name, {type: "image/png"})));
    queue.update(items[0].id, {path: "pages/renamed.png", meta: {caption: "retained"}});
    queue.move(items[1].id, 0);
    queue.remove(items[2].id);
  }, fixture);
  assert.deepEqual(await page.evaluate(() => queue.getSnapshot().items.map(item => [item.path, item.status])), [["pages/two.png", "queued"], ["pages/renamed.png", "queued"]]);
  assert.ok(await page.evaluate(() => notifications >= 4), "queue subscriptions must report actual changes");

  let pauseReached, releasePause;
  const pausedRequest = new Promise(resolve => pauseReached = resolve);
  const pauseGate = new Promise(resolve => releasePause = resolve);
  await page.route("**/v1/media/upload/presign", async route => {
    const response = await route.fetch();
    assert.equal(response.status(), 200);
    pauseReached();
    await pauseGate;
    await route.fulfill({response}).catch(() => {});
  }, {times: 1});
  await page.evaluate(() => queue.start());
  await bounded(pausedRequest, "real presign never reached the pause barrier");
  await page.evaluate(() => queue.pause());
  releasePause();
  await page.waitForFunction(() => queue.getSnapshot().items.every(item => item.status === "queued"));
  assert.equal(await page.evaluate(() => queue.getSnapshot().ready), false);

  // A real transport cut produces a failed upload; retry must use the normal
  // browser client and actual storage, without replacing its implementation.
  await page.route("**/v1/media/upload/presign", route => route.abort("connectionreset"), {times: 1});
  await page.evaluate(() => queue.start());
  await page.waitForFunction(() => queue.getSnapshot().items.some(item => item.status === "failed"));
  await page.evaluate(() => {
    const failed = queue.getSnapshot().items.find(item => item.status === "failed");
    queue.retry(failed.id);
  });
  await page.waitForFunction(() => queue.getSnapshot().ready);

  const commits = [];
  let loseCommit = true;
  await page.route("**/v1/media/upload/commit", async route => {
    commits.push(JSON.parse(route.request().postData()));
    if (loseCommit) {
      loseCommit = false;
      const response = await route.fetch();
      assert.equal(response.status(), 200, "lost response must follow an accepted real commit");
      await route.abort("connectionreset");
    } else await route.continue();
  });
  const uncertain = await page.evaluate(() => queue.commit().then(() => null, error => ({code: error.code, status: error.status})));
  assert.ok(uncertain, "accepted response was not lost");
  await page.evaluate(() => queue.commit());
  assert.equal(commits.length, 2);
  assert.deepEqual(commits[1], commits[0], "retry must preserve the full ordered operation and UUID");
  assert.match(commits[0].operation_id, /^[a-f0-9-]{36}$/);
  assert.deepEqual(await page.evaluate(() => queue.getSnapshot().items.map(item => item.status)), ["committed", "committed"]);
  const uploaded = await page.evaluate(() => client.media.read(fixture.ref, {editor: true}).then(read => read.files.filter(file => file.upload).map(file => ({path: file.path, caption: file.meta?.caption}))));
  assert.deepEqual(uploaded.map(file => file.path), ["pages/two.png", "pages/renamed.png"]);
  assert.equal(uploaded[1].caption, "retained");
  await page.unroute("**/v1/media/upload/commit");
	// A visible edition is selected for release explicitly. Publishing a
	// parent with only draft editions does not exercise the media-ready gate.
	await page.evaluate(() => client.contents.updateVersion(fixture.ref.id, fixture.ref.version, {state: "published"}));

  let accepted;
  const acceptedPublication = new Promise(resolve => accepted = resolve);
  let publicationRequests = 0;
  await page.route(`**/contents/${fixture.ref.id}/publish`, async route => {
    publicationRequests++;
    const response = await route.fetch();
    assert.equal(response.status(), 200, "publication must durably accept before media processing completes");
    const content = await response.json();
    assert.equal(content.live, false, "acceptance must not report completed publication");
    assert.equal(content.state, "published");
    accepted(content);
    await route.fulfill({response});
  });
  await page.evaluate(() => {
    window.publicationSettled = false;
    window.publication = client.contents.publishWhenReady(fixture.ref.id, undefined, 30_000)
      .then(content => { window.publicationSettled = true; return content; });
  });
  await bounded(acceptedPublication, "publishWhenReady never accepted durable work");
  assert.equal(await page.evaluate(() => publicationSettled), false, "publication completed while its media worker was stopped");
  assert.equal(await page.evaluate(() => mediaFactory.createContentKitClient({baseUrl: "/content"}).contents.get(fixture.ref.id)
    .then(() => false, error => error.status === 404)), true, "pending publication leaked to ordinary readers");
  await page.evaluate(() => fetch("/start-worker", {method: "POST"}).then(response => { if (response.status !== 204) throw Error("worker fixture did not start"); }));
  const released = await page.evaluate(() => publication.then(content => ({id: content.id, live: content.live})));
  assert.deepEqual(released, {id: fixture.ref.id, live: true});
  assert.equal(publicationRequests, 1, "completion polling resubmitted publication");
  assert.equal(await page.evaluate(() => mediaFactory.createContentKitClient({baseUrl: "/content"}).contents.get(fixture.ref.id).then(content => content.id)), fixture.ref.id);

  // Dispose aborts an in-flight multipart part; a fresh queue restores the
  // saved public state and asks real S3 which parts already landed.
  const partRequests = new Map();
  let secondPart, releasePart;
  const partReached = new Promise(resolve => secondPart = resolve);
  const partGate = new Promise(resolve => releasePart = resolve);
  let holdPart = true;
  await page.route(url => url.searchParams.has("uploadId") && url.searchParams.has("partNumber"), async route => {
    const url = new URL(route.request().url());
    const number = Number(url.searchParams.get("partNumber"));
    partRequests.set(number, (partRequests.get(number) ?? 0) + 1);
    if (number === 2 && holdPart) {
      holdPart = false;
      secondPart(); await partGate;
      await route.abort("aborted").catch(() => {});
    } else await route.continue();
  });
  await page.evaluate(() => {
    window.book = new File([new Uint8Array(65 * 1024 * 1024).fill(65)], "book.txt", {type: "text/plain"});
    window.multipart = new mediaFactory.ItemUploadQueue(client.media, {ref: fixture.ref, path: "attachments/{name}", autoStart: false, concurrency: 1, onState: (_item, state) => { if (state) window.savedUpload = structuredClone(state); }});
    multipart.add([book]); multipart.start();
  });
  await bounded(partReached, "multipart second part never reached storage", 60_000);
  const originalTicket = await page.evaluate(() => savedUpload.ticket);
  assert.ok(originalTicket);
  await page.evaluate(() => multipart.dispose());
  releasePart();
  await page.waitForFunction(() => multipart.getSnapshot().items[0].status === "queued");
  await page.evaluate(() => {
    window.resumed = new mediaFactory.ItemUploadQueue(client.media, {ref: fixture.ref, path: "attachments/{name}", autoStart: false, concurrency: 1, onState: (_item, state) => { if (state && state.ticket !== savedUpload.ticket) throw Error("multipart identity changed"); }});
    resumed.add([book], {state: () => savedUpload}); resumed.start();
  });
  await page.waitForFunction(() => resumed.getSnapshot().ready, undefined, {timeout: 60_000});
  assert.equal(partRequests.get(1), 1, "a stored multipart part was uploaded again after resume");
  assert.ok(partRequests.size > 1);
  await page.evaluate(() => resumed.commit());
  const calls = await page.evaluate(() => {
    const before = notifications;
    unsubscribe(); queue.update(queue.getSnapshot().items[0].id, {meta: {caption: "local-only"}});
    if (notifications !== before) throw Error("unsubscribe did not remove the listener");
    queue.dispose(); resumed.dispose();
    return before;
  });
  assert.ok(calls > 4);
  const bindings = ["subscribe", "getSnapshot", "add", "move", "update", "remove", "retry", "start", "pause", "commit", "dispose"].map(name => `ItemUploadQueue.${name}`);
  console.log(JSON.stringify({status: "passed", bindings, publishWhenReady: "accepted once; pending with worker stopped; completed after worker start", multipart: "same ticket; first stored part reused"}));
} finally {
  await browser.close();
}

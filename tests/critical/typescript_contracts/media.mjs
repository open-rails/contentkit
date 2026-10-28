import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  createContentKitClient,
  ContentKitError,
  fetchTransport,
} from "../../../sdk/ui/dist/client.js";

export async function runMediaContracts(group, fixture) {
  const owner = createContentKitClient({
    baseUrl: fixture.base,
    token: () => fixture.owner,
    folders: { poll: "custom_polls" },
  });
  const reader = createContentKitClient({
    baseUrl: fixture.base,
    token: () => fixture.reader,
  });
  const ref = fixture.ref,
    bytes = Buffer.from(fixture.png ?? "", "base64"),
    file = new File([bytes], "one.png", { type: "image/png" });
  const bindings = [];
  const proved = (...names) => bindings.push(...names);
  const refused = async (action, status, code) =>
    assert.rejects(
      action,
      (error) =>
        error instanceof ContentKitError &&
        error.status === status &&
        error.code === code,
    );
  const hash = (data) => createHash("sha256").update(data).digest("hex");
  if (group === "media_image_io") {
    const url =
      fixture.base + "/v1/media/gallery/" + ref.id + "/versions/" + ref.version;
    assert.equal(owner.media.api.itemURL(ref), url);
    assert.throws(
      () => owner.media.api.itemURL({ ...ref, id: "../../different-app" }),
      (error) =>
        error instanceof ContentKitError && error.code === "invalid_request",
    );
    proved("MediaApi.itemURL");
    const presets = await owner.media.api.presets();
    const cover = presets.find(
      (x) => x.kind === "gallery" && x.name === "cover",
    );
    assert(cover);
    assert.equal(cover.from, "cover");
    assert.deepEqual(cover.widths, [16, 32]);
    assert.deepEqual(await owner.media.presets(), presets);
    proved("MediaApi.presets", "MediaClient.presets");
    assert.equal(
      await owner.media.presetPath("gallery", "cover", file),
      "cover",
    );
    assert.equal(
      await owner.media.presetPath("unknown-kind", undefined, file),
      "one.png",
    );
    assert.deepEqual(owner.media.pollRef(ref.id), {
      kind: "custom_polls",
      id: ref.id,
    });
    proved("MediaClient.presetPath", "MediaClient.pollRef");
    await refused(
      () => reader.media.upload(file, { ref, path: "pages/forbidden.png" }),
      403,
      "forbidden",
    );
    const uploaded = await owner.media.upload(file, {
      ref,
      path: "pages/one.png",
    });
    assert.equal(uploaded.path, "pages/one.png");
    assert.equal(uploaded.type, "image/png");
    assert.equal(uploaded.size, bytes.length);
    assert.equal(typeof uploaded.blob, "string");
    assert(uploaded.blob.length > 0);
    proved("MediaClient.upload", "defaultTransport");
    const committed = await owner.media.commit(
      ref,
      [{ op: "put", path: uploaded.path, blob: uploaded.blob }],
      { operationID: crypto.randomUUID() },
    );
    assert(committed.some((x) => x.path === "pages/one.png" && x.upload));
    proved("MediaClient.commit");
    const processed = await owner.media.waitFor(ref, "pages/one.png", {
      timeout: 30_000,
    });
    assert.equal(processed.path, "pages/one.png");
    assert.equal(processed.w, 96);
    assert.equal(processed.h, 64);
    assert.equal(processed.size, bytes.length);
    assert(!processed.staged);
    assert(!processed.pending?.length);
    proved("MediaClient.waitFor");
    const read = await owner.media.read(ref, { editor: true });
    const source = read.files.find((x) => x.path === "pages/one.png");
    assert(source?.url);
    const stored = await fetch(source.url);
    assert.equal(stored.status, 200);
    assert.equal(hash(Buffer.from(await stored.arrayBuffer())), fixture.sha256);
    const editor = await owner.media.editorView(ref, "pages/one.png", {
      timeout: 30_000,
    });
    assert.equal(editor.width, 96);
    assert.equal(editor.height, 64);
    assert.equal((await fetch(editor.url)).status, 200);
    await refused(
      () => owner.media.editorView(ref, "pages/missing.png"),
      404,
      "not_found",
    );
    proved("MediaClient.editorView");
    const placed = await owner.media.put(file, {
      ref,
      path: "pages/two.png",
      timeout: 30_000,
    });
    assert.equal(placed.path, "pages/two.png");
    assert.equal(placed.w, 96);
    assert.equal(placed.h, 64);
    assert(placed.upload);
    proved("MediaClient.put");
    const text = new Blob(["Independent storage transport bytes"], {
      type: "text/plain",
    });
    const textBytes = Buffer.from(await text.arrayBuffer());
    const plan = await owner.media.api.presign({
      ref,
      path: "attachments/transport.txt",
      type: "text/plain",
      size: text.size,
      sha256: hash(textBytes),
    });
    assert(plan.put);
    const invalid = new URL(plan.put.url);
    invalid.searchParams.set("X-Amz-Signature", "0".repeat(64));
    await refused(
      () => fetchTransport({ ...plan.put, url: invalid.toString() }, text, {}),
      403,
      "storage",
    );
    const sent = [];
    await fetchTransport(plan.put, text, { onProgress: (n) => sent.push(n) });
    assert.deepEqual(sent, [text.size]);
    proved("fetchTransport");
    const raw = await owner.media.api.commit({
      ref,
      operation_id: crypto.randomUUID(),
      ops: [{ op: "put", path: plan.path, blob: plan.blob }],
    });
    assert(
      raw.files.some((x) => x.path === "attachments/transport.txt" && x.upload),
    );
    const attachment = await owner.media.waitFor(
      ref,
      "attachments/transport.txt",
      { timeout: 30_000 },
    );
    assert.equal(attachment.size, text.size);
    const listing = await owner.media.read(ref, {
      editor: true,
      prefix: "attachments/",
    });
    assert.equal(listing.files.length, 1);
    assert.equal(
      Buffer.from(
        await (await fetch(listing.files[0].url)).arrayBuffer(),
      ).toString(),
      "Independent storage transport bytes",
    );
    proved("MediaApi.commit");
  } else if (group === "media_named_inline") {
    const anonymous = createContentKitClient({ baseUrl: fixture.base });
    const imageRef = fixture.inlineRef;
    const named = await owner.media.uploadNamed(imageRef, file, {
      path: "named/photo.png",
      timeout: 30_000,
    });
    assert.match(named.name, /^i-[a-f0-9-]{36}$/);
    assert.equal(named.path, "named/" + named.name + ".png");
    assert.equal(named.file.path, named.path);
    assert.equal(named.file.w, 96);
    assert.equal(named.file.h, 64);
    const before = await owner.media.read(imageRef, {
      editor: true,
      prefix: "named/",
    });
    const source = before.files.find((x) => x.path === named.path);
    assert(source?.url);
    assert.equal(
      hash(Buffer.from(await (await fetch(source.url)).arrayBuffer())),
      fixture.sha256,
    );
    await owner.contents.publishWhenReady(imageRef.id, undefined, 30_000);
    const edited = await owner.media.commit(imageRef, [
      { op: "edit", path: named.path, edit: { rotate: 90 } },
    ]);
    assert(edited.some((x) => x.path === named.path && x.edit?.rotate === 90));
    await owner.media.waitFor(imageRef, named.path, {
      timeout: 30_000,
    });
    const rotated = (
      await owner.media.read(imageRef, {
        editor: true,
        prefix: "named-reader/",
      })
    ).files.find((x) => x.path === "named-reader/" + named.name + ".webp");
    assert(rotated);
    assert.equal(rotated.w, 32);
    assert.equal(rotated.h, 48);
    assert.equal(
      hash(Buffer.from(await (await fetch(source.url)).arrayBuffer())),
      fixture.sha256,
    );
    assert.equal(
      (await anonymous.contents.get(imageRef.id)).decision.allowed,
      true,
    );
    proved("MediaClient.uploadNamed");
    const inline = await owner.media.uploadInline(imageRef, file, {
      timeout: 30_000,
    });
    assert.match(inline.name, /^i-[a-f0-9-]{36}$/);
    assert.equal(inline.path, "body/" + inline.name + ".png");
    assert.equal(inline.ref, "contentkit:" + inline.name);
    // The editor view renders once an editor read asks for it, so it may not
    // exist yet when the upload settles (url is null until then).
    const preview = inline.url ?? (await owner.media.editorView(imageRef, inline.path)).url;
    assert.equal(typeof preview, "string");
    assert.equal((await fetch(preview)).status, 200);
    await owner.contents.updateVersion(imageRef.id, imageRef.version, { text: { en: {
      title: "Inline body",
      body: "Text ![inline](" + inline.ref + ")",
    } } });
    // Only the completed ordinary reader projection proves publication. An
    // uploaded/processed image and a saved body can precede that durable move.
    const publishedInline = async (previous) => {
      const deadline = Date.now() + 30_000;
      do {
        const content = await anonymous.contents.get(imageRef.id);
        const url = content.images?.[inline.name];
        if (typeof url === "string" && url !== previous) return content;
        await new Promise((resolve) => setTimeout(resolve, 50));
      } while (Date.now() < deadline);
      assert.fail("inline image did not reach the completed reader projection");
    };
    const exposed = await publishedInline();
    assert.equal(exposed.decision.allowed, true);
    assert.equal(typeof exposed.images[inline.name], "string");
    assert(exposed.body.includes(exposed.images[inline.name]));
    assert(!exposed.body.includes(inline.ref));
    const selected = exposed.versions.find((version) => version.id === imageRef.version);
    assert.equal(selected.body, exposed.body);
    assert.equal(selected.images[inline.name], exposed.images[inline.name]);
    const authored = await owner.contents.version(imageRef.id, imageRef.version);
    assert.equal(authored.text.en.body, "Text ![inline](" + inline.ref + ")", "rendering must not overwrite the stored image reference");
    const exact = await anonymous.contents.getByVersion(imageRef.version, "en");
    assert.equal(exact.selected_version_id, imageRef.version);
    assert(exact.body.includes(exact.images[inline.name]), "direct edition reads must resolve their own inline images too");
    const bodyOriginal = (
      await owner.media.read(imageRef, { editor: true, prefix: "body/" })
    ).files.find((x) => x.path === inline.path);
    assert(bodyOriginal?.url);
    await owner.media.commit(imageRef, [
      { op: "edit", path: inline.path, edit: { rotate: 90 } },
    ]);
    await owner.media.waitFor(imageRef, inline.path, {
      timeout: 30_000,
    });
    const bodyRendition = (
      await owner.media.read(imageRef, { editor: true, prefix: "body-reader/" })
    ).files.find((x) => x.path === "body-reader/" + inline.name + ".webp");
    assert.equal(bodyRendition.w, 32);
    assert.equal(bodyRendition.h, 48);
    assert.equal(
      hash(Buffer.from(await (await fetch(bodyOriginal.url)).arrayBuffer())),
      fixture.sha256,
    );
    const after = await publishedInline(exposed.images[inline.name]);
    assert.equal(after.decision.allowed, true);
    assert(after.body.includes(after.images[inline.name]));
    assert(!after.body.includes(exposed.images[inline.name]), "a processed edit must refresh the URL in the rendered body");
    proved("MediaClient.uploadInline");
    await owner.media.put(file, {
      ref,
      path: "pages/one.png",
      timeout: 30_000,
    });
    await owner.contents.publishWhenReady(ref.id, undefined, 30_000);
    let download;
    const deadline = Date.now() + 30_000;
    do {
      download = await owner.versions.download(ref.id, ref.version);
      if ("url" in download) break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    } while (Date.now() < deadline);
    assert.equal(typeof download.url, "string");
    assert.equal(new URL(download.url).origin, new URL(fixture.base).origin);
    assert(new URL(download.url).pathname.endsWith("/archive"));
    assert.equal(download.type, "application/zip");
    assert(download.name.endsWith(".zip"));
    assert(Date.parse(download.expires_at) > Date.now());
    const zipped = await fetch(download.url);
    assert.equal(zipped.status, 200);
    const zip = Buffer.from(await zipped.arrayBuffer());
    assert(zip.length > 100);
    assert.deepEqual([...zip.subarray(0, 4)], [80, 75, 3, 4]);
    proved("VersionsClient.download");
    await owner.contents.updateVersion(imageRef.id, imageRef.version, {
      access: { level: "tier", tiers: ["gold"] },
    });
    let locked;
    const privateDeadline = Date.now() + 30_000;
    do {
      const edition = await owner.contents.version(imageRef.id, imageRef.version);
      locked = await anonymous.contents.get(imageRef.id);
      if (edition.publication?.state === "completed" && !locked.decision.allowed) break;
      await new Promise((resolve) => setTimeout(resolve, 50));
    } while (Date.now() < privateDeadline);
    assert.equal(locked.decision.allowed, false);
    assert.equal(locked.body ?? "", "");
    assert.deepEqual(locked.images ?? {}, {});
  } else if (group === "media_abort") {
    const size = 65 << 20;
    const sha256 = hash(Buffer.alloc(size, 97));
    for (const mode of ["api", "client"]) {
      const plan = await owner.media.api.presign({
        ref,
        path: "attachments/" + mode + ".txt",
        type: "text/plain",
        size,
        sha256,
      });
      assert(plan.multipart?.ticket);
      assert(!plan.put);
      await refused(
        () => reader.media.api.abort(ref, { ticket: plan.multipart.ticket }),
        403,
        "forbidden",
      );
      assert.deepEqual(
        (
          await owner.media.api.listParts(ref, {
            ticket: plan.multipart.ticket,
          })
        ).parts,
        [],
      );
      if (mode === "api")
        await owner.media.api.abort(ref, { ticket: plan.multipart.ticket });
      else
        await owner.media.discard({
          ref,
          ticket: plan.multipart.ticket,
          path: plan.path,
          blob: plan.blob,
          type: "text/plain",
          size,
          file: { name: "client.txt", size, lastModified: 0 },
          parts: [],
          limits: {
            minPartSize: plan.multipart.min_part_size,
            maxPartSize: plan.multipart.max_part_size,
            maxParts: plan.multipart.max_parts,
          },
        });
      await refused(
        () => owner.media.api.complete(ref, { ticket: plan.multipart.ticket }),
        404,
        "not_found",
      );
      await refused(
        () =>
          owner.media.api.commit({
            ref,
            operation_id: crypto.randomUUID(),
            ops: [{ op: "put", path: plan.path, blob: plan.blob }],
          }),
        409,
        "not_uploaded",
      );
      assert.equal(
        (await owner.media.read(ref, { editor: true })).files.length,
        0,
      );
    }
    proved("MediaApi.abort", "MediaClient.discard");
  } else if (group === "media_hls_transport") {
    const expected =
      fixture.base +
      "/v1/media/" +
      ref.kind +
      "/" +
      ref.id +
      "/versions/" +
      ref.version +
      "/hls/streams/";
    assert.equal(owner.media.hlsBase(ref, "streams/"), expected);
    proved("MediaClient.hlsBase");
    const master = await owner.media.hls.playlist(expected + "master.m3u8");
    assert.equal(master.status, 200);
    const masterBody = await master.text();
    assert(masterBody.includes("#EXT-X-STREAM-INF"));
    assert(masterBody.includes("avc1"));
    const childURL = owner.media.api.itemURL(ref) + "/hls/" + fixture.child;
    const child = await owner.media.hls.playlist(childURL);
    assert.equal(child.status, 200);
    const childBody = await child.text();
    assert(childBody.includes("#EXT-X-BYTERANGE:"));
    assert(childBody.includes("#EXT-X-MAP:"));
    const denied = await reader.media.hls.playlist(childURL);
    assert.equal(denied.status, 404);
    await denied.body?.cancel();
    const segment = await owner.media.hls.playlist(fixture.segment);
    assert.equal(segment.status, 200);
    assert((await segment.arrayBuffer()).byteLength > 100);
    const empty = await owner.contents.create({
      kind: "clip",
      versions: [{ languages: ["en"], text: { en: { title: "Native browser transport upload" } } }],
    });
    const freshRef = {
      kind: "clip",
      id: empty.id,
      version: empty.versions[0].id,
    };
    const payload = Buffer.from(fixture.source, "base64");
    assert.equal(hash(payload), fixture.sourceSHA);
    const plan = await owner.media.api.presign({
      ref: freshRef,
      path: "source.mp4",
      type: "video/mp4",
      size: payload.length,
      sha256: fixture.sourceSHA,
    });
    assert(plan.put);
    const { runBrowserTransports } = await import("./transport-browser.mjs");
    await runBrowserTransports(fixture, plan);
    proved("MediaClient.hls", "xhrTransport");
  } else throw new Error("Unknown media contract " + group);
  return bindings;
}

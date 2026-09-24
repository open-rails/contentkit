# @open-rails/contentkit-upload

Browser client for ContentKit's `media.UploadHandler`: SHA-256-bound single
PUTs up to 64 MiB, resumable multipart above (8–16 MiB parts sized from
measured throughput, bounded concurrency, per-part retry), commit, and React
hooks.

```sh
pnpm add @open-rails/contentkit-upload
```

The host mounts `media.UploadHandler` (e.g. at `/api/media/upload`) behind its
auth, and the bucket allows CORS `PUT` from the app's origin with the
`Content-Type` and `x-amz-checksum-sha256` headers.

## Core

```ts
import { createUploadClient } from "@open-rails/contentkit-upload";

const client = createUploadClient({ endpoint: "/api/media/upload" });
const ref = { kind: "gallery", id: "123", version: "en" };

const up = await client.upload(file, { ref, onProgress: (p) => console.log(p.phase, p.loaded, p.total) });
await client.commit(ref, [{ op: "insert", name: "001.png", original: up.name }], {
  sources: { [up.name]: file }, // re-upload and retry once if the original went stale (not_uploaded)
});

await client.uploadSlot(cover, { ref, slot: "cover" }); // upload + commit-slot

await client.edit(ref, "001.png", { crop: { x: 0, y: 0, w: 800, h: 600 }, rotate: 90 }); // null clears
await client.setSlotFromFile(ref, "cover", "001.png", { crop: { x: 40, y: 0, w: 460, h: 0 } }); // slot aspect sets h

// A new inline image (post bodies, poll options): the server names it i-{uuid}.
const img = await client.uploadInline(file, { ref: { kind: "post", id: postId } });
// then e.g. POST /posts/{id}/images {"image": img.name} -> {"url"}
```

- Errors are `UploadError` with `code` (the server's `ErrorReply.code`, or
  `network`, `storage`, `aborted`, `resume_mismatch`), `status` and
  `retryAfter` (seconds, on `rate_limited`). `isLimit` is true for
  `rate_limited` and `quota_exceeded`; `isCeiling` for `too_many_files` (409,
  the kind's file caps).
- A refused presign throws before any bytes move; multipart files are
  presigned before they are hashed.
- Aborting `signal` pauses a multipart upload. `onState` reports resumable
  state (JSON; `null` when done); pass it back as `resume` with the same file
  to continue after a reload, or `client.discard(state)` to drop it.

## React

```tsx
import { useUploadQueue, useUpload } from "@open-rails/contentkit-upload/react";

const q = useUploadQueue(client, { ref });
q.add(input.files!);           // uploads in the background, 2 at a time
q.move(id, 0);                 // reorder before commit
q.update(id, { name: "001.png" });
q.blocked;                     // rate/quota/permission refusal: nothing new starts until q.start()
await q.commit();              // inserts uploaded files in queue order; re-uploads stale ones

const cover = useUpload(client);
await cover.upload(file, { ref, slot: "cover" }); // or { ref, inline: true }
```

`UploadQueue` is the framework-free queue behind the hook.

`useCrop` is headless crop state for any cropper UI; crops are in original
pixels (`dims` from the read API), before a clockwise rotation:

```tsx
const c = useCrop({ source: { width: dims.w, height: dims.h }, aspect: 460 / 650, initial: file.edit });
<AnyCropper onChange={(rect) => c.setFromDisplay(rect, { width: img.width, height: img.height })} />;
c.rotateBy(90);
await client.setSlotFromFile(ref, "cover", name, c.edit ?? {});
```

`constrainCrop`, `toOriginal`, `centeredCrop` and `editOf` are the same math
without React.

## Development

Wire types (`src/wire.gen.ts`) are generated from the Go handler:
`go test ./media/internal/wirets -update`.

```sh
pnpm test               # unit
CONTENTKIT_TEST_S3_ENDPOINT=http://localhost:9000 CONTENTKIT_TEST_S3_ACCESS_KEY=… \
CONTENTKIT_TEST_S3_SECRET_KEY=… pnpm test:integration   # real handler + MinIO
```

Published on `v*` tags with the ContentKit version.

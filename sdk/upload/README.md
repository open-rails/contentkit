# @openrails/contentkit-upload

Browser client for ContentKit's `media.UploadHandler`: SHA-256-bound single
PUTs up to 64 MiB, resumable multipart above (8–16 MiB parts sized from
measured throughput, bounded concurrency, per-part retry), commit, and React
hooks, and a styled UI for cropped slot images (avatars, covers).

Each ContentKit release (`v*`) attaches the package as a release asset:

```sh
pnpm add https://github.com/open-rails/contentkit/releases/download/v0.21.0/openrails-contentkit-upload-0.21.0.tgz
```

The host mounts `media.UploadHandler` (e.g. at `/api/media/upload`) behind its
auth with `UploadHandlerOptions.PublicBaseURL` set (the access worker origin slot
manifests build output URLs on; slot routes fail without it), and the bucket allows CORS `PUT` from the app's origin with the
`Content-Type` and `x-amz-checksum-sha256` headers.

## Core

```ts
import { createUploadClient } from "@openrails/contentkit-upload";

const client = createUploadClient({ endpoint: "/api/media/upload" });
const ref = { kind: "gallery", id: "123", version: "en" };

const up = await client.upload(file, { ref, onProgress: (p) => console.log(p.phase, p.loaded, p.total) });
await client.commit(ref, [{ op: "insert", name: "001.png", original: up.name }], {
  sources: { [up.name]: file }, // re-upload and retry once if the original went stale (not_uploaded)
});

await client.uploadSlot(cover, { ref, slot: "cover", edit }); // upload + commit-slot → { manifest }
await client.editSlot(ref, "cover", edit);                   // re-render from the committed original
await client.getSlot(ref, "cover");                          // { aspect, edit, dims, version, outputs, pending }

await client.edit(ref, "001.png", { crop: { x: 0, y: 0, w: 800, h: 600 }, rotate: 90 }); // null clears
await client.setSlotFromFile(ref, "cover", "001.png", { crop: { x: 40, y: 0, w: 460, h: 0 } }); // slot aspect sets h

// A new inline image (post bodies, poll options): the server names it i-{uuid}.
const img = await client.uploadInline(file, { ref: { kind: "post", id: postId } });
// then e.g. POST /posts/{id}/images {"image": img.name} -> {"url"}
```

A slot `edit` is the same `Edit` as files: a crop in pixels of the EXIF-oriented
original, then a clockwise `rotate`; the server derives `h` from `w` and the slot's aspect,
and no crop means the largest centred one. Output URLs carry `?v={version}` and are
immutable per version. `slotSources(manifest)` gives `src`/`srcSet`; `waitForSlot` polls
while `pending`; `getSlotOriginal` returns the committed original for re-editing;
`decodeImage` is the EXIF-aware preview the UI crops on.

Video items (#32): a poster (slot `poster`, 16:9) and a hover preview.

```ts
await client.getVideoImages(ref);                                  // { poster, hover_preview, video: { file, duration, w, h, encoded } }
await client.setVideoPoster(ref, { source: "frame", time: 8.3, edit }); // edit in frame pixels (video.w × video.h)
await client.uploadVideoPoster(image, { ref, edit });              // presign slot "poster" + /video-poster upload
await client.setVideoPoster(ref, { source: "auto" });
await client.setHoverPreview(ref, { start: 3, duration: 4 });     // {} = automatic
await client.getFrame(ref, 8.3, { width: 640 });                   // JPEG Blob (GET /frame)
await client.waitForVideoImages(ref);                              // until nothing is pending
```

- Errors are `UploadError` with `code` (the server's `ErrorReply.code`, or
  `network`, `storage`, `aborted`, `resume_mismatch`), `status` and
  `retryAfter` (seconds, on `rate_limited`). `isLimit` is true for
  `rate_limited` and `quota_exceeded`; `isCeiling` for `too_many_files` (409,
  the kind's file caps).
- A refused presign throws before any bytes move; multipart files are
  presigned before they are hashed.
- Parts hash with WebCrypto (off the main thread; `@noble/hashes` in an
  insecure context) and are hashed and presigned ahead of the PUT slots, so the
  link stays busy. `bench/run.ts` measures a 1 GB upload from Chromium.
- Aborting `signal` pauses a multipart upload. `onState` reports resumable
  state (JSON; `null` when done); pass it back as `resume` with the same file
  to continue after a reload, or `client.discard(state)` to drop it.

## React

```tsx
import { useUploadQueue, useUpload } from "@openrails/contentkit-upload/react";

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

`c.edit` keeps its identity until its value changes, so it can feed effects
and state setters (`ImageCropDialog`'s `onEditChange` fires only on a change).
`constrainCrop`, `toOriginal`, `centeredCrop`, `editOf` and `sameEdit` are the
same math without React.

Headless slot hooks:

```tsx
const img = useSlotImage(client, { ref, slot: "avatar" });   // manifest, src, srcSet, aspect, reload
<img src={img.src} srcSet={img.srcSet} sizes="96px" />

const crop = useSlotCrop(client, { ref, slot: "avatar", manifest: img.manifest, onSaved: img.set });
crop.pick(file);     // idle → decoding → cropping { source, edit, mode: "new" }
await crop.recrop(); // cropping from the committed original via editSlot (mode: "recrop"); crop.canRecrop
crop.setEdit(e);     // Edit: crop in oriented-original pixels, then a clockwise rotate
await crop.save();   // saving { progress, rendering } → done { manifest } | error { error, source } (retry with save())
crop.cancel();
```

Headless video hooks: `useVideoImages(client, { ref, file?, images? })`,
`useFrameStrip(client, { ref, duration, count, width })` (frames fetched one at
a time), `useVideoFrame(client, { ref, time, width, delay })` (debounced exact
frame), `useVideoPoster(client, { ref, onSaved })` (`saveFrame(time, edit)`,
`saveUpload(blob, edit)`, `saveAuto()`, `state`) and `useHoverSection(client,
{ ref, duration, initial, onSaved })` (`start`, `length`, bounded setters,
`save()`, `saveAuto()`).

Video encode progress: the read API puts `progress` (`EncodeProgress`) on a
pending video file; `useEncodeProgress(file.progress)` counts its ETA down
between polls (`{ progress, remaining }`).

## UI

`@openrails/contentkit-upload/ui`: shadcn (base-vega, Base UI, zinc) components
whose CSS is scoped under `.ckui` and installed on import (also shipped as
`./styles.css`). Crop the picked image in a dialog (drag, wheel/pinch/slider zoom,
arrow keys and +/−, 90° rotation), upload the original with the crop, and let the server
render every size; show a post's media with `MediaGallery` and `VideoPlayer`.

```tsx
import { AvatarUpload, CoverUpload, SlotImage, UploadUiProvider } from "@openrails/contentkit-upload/ui";
import { ja } from "@openrails/contentkit-upload/locales/ja";

<UploadUiProvider client={client} messages={ja} appearance={{ theme: "inherit" }}>
  <CoverUpload item={{ kind: "channel", id }} onChange={(m) => save(m)} />
  <AvatarUpload item={{ kind: "channel", id }} />
  <SlotImage item={{ kind: "user", id }} slot="avatar" round sizes="40px" className="size-10" />
</UploadUiProvider>
```

`AvatarUpload` / `CoverUpload` are complete form fields. Layouts that draw the
images themselves (a channel header with icon buttons over the cover) use
`SlotEditor`: the same pick → crop → save / re-crop flow with no markup of its
own. Its children draw the image from `useSlotEditor()` and put triggers where
they belong; `SlotEditMenu` is one trigger (a file picker, or a Change / Edit crop
menu once the slot keeps an original) and takes a host-styled element via `render`:

```tsx
import { SlotEditError, SlotEditMenu, SlotEditor, SlotImage, useSlotEditor } from "@openrails/contentkit-upload/ui";

function Cover() {
  const { image } = useSlotEditor();
  return <SlotImage manifest={image.manifest} sizes="100vw" />;
}

<SlotEditor item={channel} slot="cover" manifest={channel.cover} aspect={3} onChange={saveCover}>
  <Cover />
  <SlotEditMenu label="Change cover" iconOnly render={<button className="my-overlay-button" />} />
  <SlotEditError />
</SlotEditor>
```

| Component | Props |
| --- | --- |
| `AvatarUpload`, `CoverUpload` | `item`, `slot` (default `avatar`/`cover`), `client`, `manifest` (else fetched), `onChange(manifest)`, `aspect` (default the manifest's, else 1 / 3), `targetWidth` (sharpness warning below it; default 512 / 3000), `accept`, `sizes`, `disabled`, `label`, `hint` |
| `SlotEditor` | `item`, `slot`, `client`, `manifest` (else fetched), `onChange(manifest)`, `aspect` (default the manifest's, else 1), `targetWidth` (default the widest output), `round` (default aspect 1), `title`, `accept`, `disabled`, `children` |
| `useSlotEditor()` | `{ image, crop, has, busy, disabled, error, choose(), pick(file), recrop() }` inside a `SlotEditor` |
| `SlotEditMenu` | `label`, `iconOnly`, `render` (trigger element; default the kit's outline button), `className`, `align`; the trigger has `data-ckui="slot-edit"` and `data-busy` |
| `SlotEditError` | `className`: the editor's error outside the dialog |
| `ImageCropDialog` | `open`, `onOpenChange`, `source` (`{ url, width, height }` of the oriented original), `aspect`, `round`, `initialEdit`, `onEditChange`, `onConfirm(edit)`, `targetWidth`, `busy`, `progress`, `error`, `title` |
| `EncodeProgress` | `progress` (a read API file's `progress`; absent shows "Processing video"), `className`, `appearance`: bar, phase, `segment 5 / 27`, `~40 s left` or queue position; `data-ckui="encode-progress"`, `data-phase` |
| `SlotImage` | `manifest` or `item` + `slot`, `sizes`, `round`, `aspect`, `placeholder`, `alt` |
| `VideoPosterPicker` | `open`, `onOpenChange`, `item`, `file`, `client`, `images` (else fetched), `onChange(images)`, `title`, `accept`: frame strip + slider + frame steps over `/frame`, "Use this frame", "Crop…" (in `video.w×h` pixels), "Upload image" → `ImageCropDialog`, "Automatic" |
| `HoverPreviewPicker` | same props: a 1–6 s range over the frame strip, an approximate flip-book of the section, the rendered loop once saved, "Automatic" |
| `VideoPoster` | `poster` (`VideoImages.poster` or a listing's outputs), `preview` (`hover_preview` or `{ mp4, webp }` URLs), `playing` (default hover or focus within), `sizes`, `alt`, `children`: 16:9 `srcset` poster that plays the preview |
| `HoverPreview` | `preview`, `active`, `width`: muted looping MP4 (`playsinline`), WebP on error; nothing with `prefers-reduced-motion` |
| `UploadUiProvider` | `client`, `appearance` (`theme`: `light`/`dark`/`auto`/`inherit`, `variables`), `messages` (bundle or list; locales `en de es ja ko zh`), `t` (host translate hook) |

Errors are mapped from `UploadError.code` to `errors.*` messages.

### Media gallery and player

`MediaGallery` renders a read API result: an Instagram-style carousel (swipe,
arrows, ←/→, dots, counter) or a tile grid whose tiles open a lightbox carousel
(Esc closes, focus is trapped and returns to the tile), with a view toggle in
its header. One item renders alone. The carousel spans its column at the first
item's aspect (9:16 to 2.4:1), capped at `maxHeight`, and letterboxes the rest;
only the current slide and its neighbours are mounted, and a video swiped away
pauses. Viewers without access see the blurred teaser behind one locked item
with the host's `renderLocked`; locked files carry no URLs.

```tsx
<MediaGallery
  read={read}                                    // GET /{kind}/{id}?variant=large,blurred
  hlsBase={(f) => `/api/media/post/${id}/hls/${encodeURIComponent(f.name!)}/`}
  xhrSetup={(xhr) => xhr.setRequestHeader("Authorization", `Bearer ${token()}`)} // same-origin playlists only
  refresh={() => refetchRead()}                  // after a 401/403: re-grant, then the player retries once
  videoImages={images}                           // optional poster + hover preview (GET …/video-images)
  renderLocked={({ count }) => <UnlockButton count={count} />}
  renderDetails={(item) => <Downloads item={item} />}
  defaultView="carousel"                         // or view + onViewChange; storageKey remembers the choice
/>
```

`VideoPlayer` (`base`, `width`/`height` reserve the box, `poster`, `duration`,
`pending`/`progress` show `EncodeProgress`, `failed`, `layout` `frame`|`fill`,
`maxHeight` default `80svh`, `active`, `xhrSetup`, `refresh`) loads nothing
until played (hls.js imported then; native HLS on Safari), and never spins
forever: tuned retries surface a dead endpoint within seconds, a watchdog
catches 10 s without progress, and each failure has its own message, a Retry
and a support code: unreachable or blocked (status 0, including missing
`MEDIA_ACCESS_CORS_ORIGINS`, also logged to the console), no access
(401/403 after one refresh), not found, rate limited (429), unsupported in
this browser. Headless: `useHlsPlayer`, `useCarousel` and `useGalleryView` in
`/react`; `galleryItems`, `classifyHlsError`, `hlsConfig` and the ABR helpers
(`capRung`, `startRung`, `initialEstimate`, `abrHlsConfig`) in the root entry.

Adaptive bitrate (`abr` on `MediaGallery`, `VideoPlayer` and `useHlsPlayer`)
defaults to high resolution: the first segment is the highest rung the
connection sustains up to 1080p on a phone or small player (the estimate is
this page's last measurement, else `navigator.connection.downlink`, else
8 Mbps), and the display cap counts `devicePixelRatio` but never drops below
1080p, so 1440p/2160p load only when the player (fullscreen, lightbox, a 4K or
high-DPR screen) and bandwidth allow. It climbs after a couple of fast
segments and drops before the buffer runs dry. Save-Data starts at the lowest
rung with a strict display cap. Options: `minStartHeight` (1080),
`maxHeight` (none), `defaultEstimate` (8e6 bits/s), `preferHighRes` (`false`
restores stock hls.js: 500 kbps guess, strict cap). Heights are the short side,
so portrait 1080×1920 is 1080p.

Once playing, a quality menu (top right; keyboard accessible) lists Auto,
showing the rung playing (“Auto (1080p)”), and every rung (“2160p 4K”,
“1440p”, “1080p HD”, “720p”, “480p”). A rung locks until Auto is chosen; the
choice is remembered per browser (`qualityKey`, default
`ckui.player.quality`; `null` disables) and falls back to Auto on a video
without that rung, so rungs published later are picked up on the next load.
Safari's native HLS chooses for itself, so it shows no menu. Headless:
`useHlsPlayer().quality` (`levels`, `selected`, `current`, `select`).

## Development

Wire types (`src/wire.gen.ts`) are generated from the Go handler:
`go test ./media/internal/wirets -update`.

```sh
pnpm test               # unit + jsdom hooks/components
CONTENTKIT_TEST_S3_ENDPOINT=http://localhost:9000 CONTENTKIT_TEST_S3_ACCESS_KEY=… \
CONTENTKIT_TEST_S3_SECRET_KEY=… pnpm test:integration   # real handler + MinIO
pnpm build && pnpm screenshots   # demo/ in Chromium, light/dark × desktop/mobile (SCREENSHOT_DIR)
```

UI primitives come from `pnpm dlx shadcn@4.21.0 add …` (`components.json`); local
edits are marked `// Local:`.

The `sdk-release` workflow packs `dist` (`pnpm pack`) onto every published `v*` release, versioned by the tag.

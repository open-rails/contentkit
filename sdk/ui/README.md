# @openrails/contentkit-upload

Browser client for ContentKit media: staged uploads (one PUT up to
64 MiB, resumable multipart above: 8–16 MiB parts sized from measured
throughput, bounded concurrency, per-part retry), commit ops, the read API,
React hooks, and a styled UI for cropped images, video posters and galleries.

Each ContentKit release (`v*`) attaches the package as a release asset:

```sh
pnpm add https://github.com/open-rails/contentkit/releases/download/v0.62.0/openrails-contentkit-upload-0.62.0.tgz
```

The host mounts `media.UploadHandler` (e.g. at `/api/media/upload`) and
`Reader.Handler` (e.g. at `/api/media`) behind its auth, and the bucket allows
CORS `PUT` from the app's origin with the `Content-Type` and
`x-amz-checksum-sha256` headers. Paths, kinds and public presets are the app's
`media.Config` registry; a ref is `{ kind, id }` (an item is the host's version).

## Core

```ts
import { createUploadClient, publicURL, srcSet } from "@openrails/contentkit-upload";

const client = createUploadClient({ endpoint: "/api/media/upload", readEndpoint: "/api/media" });
const ref = { kind: "gallery", id: versionId };

// Hashes the whole file (in a Web Worker), presigns {ref, path, type, size, sha256}, sends it.
const up = await client.upload(file, { ref, path: "originals/001.png", onProgress: (p) => console.log(p.phase, p.loaded, p.total) });
// up: { path: "originals/001.png", blob: "sha256-…", type, size, exists }
await client.commit(ref, [{ op: "put", path: up.path, blob: up.blob }], {
  sources: { [up.blob]: file }, // re-upload and retry once if the blob went stale (not_uploaded)
});

// upload + put + wait until the worker processed it; resolves with the upload as an editor reads it.
const cover = await client.put(image, { ref, path: "cover", edit: { crop: { x: 0, y: 40, w: 1200, h: 0 } } });
await client.commit(ref, [{ op: "edit", path: cover.path, edit }]); // also move, rename, remove, attach, copy, frame, meta, regenerate
await client.waitFor(ref, "cover");                                 // until nothing is pending; rejects with its failure

const read = await client.read(ref, { prefix: "low-res/", offset: 0, limit: 50, download: true }); // editor: true adds the uploads
const view = await client.editorView(ref, "cover");                // { url, width, height } to re-crop on (editors)
const still = await client.getFrame(ref, "source.mp4", 8.3, 640);  // JPEG Blob (GET /frame)
const base = client.hlsBase(ref, read.hls![0]!);                   // {readEndpoint}/{kind}/{id}/hls/{dir}: master.m3u8, sprite.vtt

// Public files sit at fixed names, as media.PublicURL and media.SrcSet build them.
publicURL("https://media.doujins.ai", "doujins", "gallery", id, "cover-460.webp");
srcSet("https://media.doujins.ai", "doujins", "gallery", id, "cover-{w}.webp", [230, 460, 920]);
```

- An inline image is a put to a Named upload path (`inline/x.png`): the server
  names it `inline/i-{uuid}.png`, and its public URL is `publicURL(…)` of the
  kind's public preset name, e.g. `fill("{name}.webp", { name: "i-…" })`.
- A video poster is an upload whose `Frames` is the video: `frame { path, t }`
  or `{ path, auto: true }` grabs it, a put uploads one.
- An `edit` is a crop in pixels of the EXIF-oriented upload, then a clockwise
  `rotate`; the server fits `h` to the public preset's aspect (`"W:H"`;
  `ratio("3:1")` gives the number, `aspectOf(w, h)` the reduced string), and no
  crop means the largest centred one. `decodeImage` is the EXIF-aware preview
  of a picked file.
- After a change, `reloadPublic(urls)` refetches public files past the browser
  cache and remounts the kit's images showing them (the kit does this after its saves).
- Errors are `UploadError` with `code` (the server's `ErrorReply.code`, or
  `network`, `storage`, `aborted`, `resume_mismatch`, `decode`,
  `render_timeout`), `status`, `retryAfter` (seconds, on `rate_limited`),
  `blobs` (`not_uploaded`) and `details` (image refusals: `image_too_small`
  `{ width, min_width }`, `image_too_large`, `image_unreadable`,
  `type_not_allowed` `{ allowed }`, `too_large` `{ size, max_bytes }`,
  `animation_not_allowed`, `animation_too_long`, `animation_unsupported`).
  An upload the worker cannot process carries `failed` in editor reads;
  `failureError(failed)` is its error. `refusal` is true when the request broke a
  stated rule rather than hitting a fault; `isLimit` for `rate_limited` and
  `quota_exceeded`; `isCeiling` for `too_many_files`.
- Hashing: files up to 64 MiB and every part use WebCrypto; larger files
  stream through `@noble/hashes` in an inline Web Worker (on the main thread
  where workers cannot start). Parts hash and presign ahead of the PUT slots.
  `bench/run.ts` measures a 1 GB upload from Chromium.
- Aborting `signal` pauses a multipart upload. `onState` reports resumable
  state (JSON; `null` when done); pass it back as `resume` with the same file
  to continue after a reload (nothing is hashed again), or `client.discard(state)`.

## React

```tsx
import { useRead, useUpload, useUploadQueue } from "@openrails/contentkit-upload/react";

const q = useUploadQueue(client, { ref, path: "originals/{name}" });
q.add(input.files!);           // uploads in the background, 2 at a time
q.move(id, 0);                 // reorder before commit
q.update(id, { path: "originals/001.png", meta: { alt: "Cover page" } });
q.blocked;                     // rate/quota/permission refusal: nothing new starts until q.start()
await q.commit();              // puts the uploads after the item's, in queue order; re-uploads stale ones

const one = useUpload(client);
await one.upload(file, { ref, path: "inline/x.png", put: {} }); // put: commit it and wait (result.file)

const { read, reload } = useRead(client, ref, { prefix: "low-res/" });
```

Queue puts are create-only: a canonical filename collision is refused rather
than replacing another upload. For a standalone new-file upload, use
`client.put(file, { ref, path, createOnly: true })`. Ordinary `put` still replaces
by stem; cover/avatar editors retain that behavior.

With the host's `ProcessOnUpload`, the queue commits each upload unattached as
it lands, polls an editor read until it is processed, and `commit` attaches them.

`useCrop` is headless crop state for any cropper UI; crops are in upload
pixels (an editor read's `w`, `h`), before a clockwise rotation:

```tsx
const c = useCrop({ source: { width: file.w, height: file.h }, aspect: "46:65", initial: file.edit });
<AnyCropper onChange={(rect) => c.setFromDisplay(rect, { width: img.width, height: img.height })} />;
c.rotateBy(90);
await client.commit(ref, [{ op: "edit", path: file.path, ...(c.edit ? { edit: c.edit } : {}) }]);
```

`c.edit` keeps its identity until its value changes. `constrainCrop`,
`toOriginal`, `centeredCrop`, `editOf` and `sameEdit` are the same math without React.

Headless image hooks:

```tsx
const img = useSlotImage(client, { ref, path: "avatar", image });   // file (the upload), renditions, aspect, reload, set
const crop = useSlotCrop(client, { ref, path: "avatar", file: img.file, aspect: "1:1", image, onSaved: img.set });
crop.pick(file);     // idle → decoding → cropping { source, edit, mode: "new" }
await crop.recrop(); // cropping the committed upload's editor view (mode: "recrop"); crop.canRecrop
await crop.save();   // saving { progress, rendering } → done { file } | error { error, source } (retry with save())
```

`image` is a `PublicImage`: the item's public preset, `{ base, namespace, kind,
id, to: "avatar-{w}.webp", widths, aspect }`.

Headless video hooks: `useVideoImages(client, { ref, video: "source", poster: "poster" })`
(the two uploads), `useFrameStrip(client, { ref, path, duration, count, width })`
(frames fetched one at a time), `useVideoFrame(client, { ref, path, time, width, delay })`
(debounced exact frame), `useVideoPoster(client, { ref, path, image, onSaved })`
(`saveFrame(time, edit)`, `saveUpload(blob, edit)`, `saveAuto()`, `state`).

Encode progress: an editor read puts `progress` (`EncodeProgress`) on a pending
video upload; `useEncodeProgress(file.progress)` counts its ETA down between
polls (`{ progress, remaining }`).

## UI

`@openrails/contentkit-upload/ui`: shadcn (base-vega, Base UI, zinc) components
whose CSS is scoped under `.ckui` and installed on import (also shipped as
`./styles.css`). Crop the picked image in a dialog (drag, wheel/pinch/slider zoom,
arrow keys and +/−, 90° rotation), upload it with the crop, and let the worker
render the public sizes; show a post's media with `MediaGallery` and `VideoPlayer`.

```tsx
import { AvatarUpload, CoverUpload, SlotImage, UploadUiProvider } from "@openrails/contentkit-upload/ui";
import { ja } from "@openrails/contentkit-upload/locales/ja";

const avatar = { base: MEDIA, namespace: "accounts", kind: "user", id: userId, to: "avatar-{w}.webp", widths: [64, 128, 256, 512], aspect: "1:1" };

<UploadUiProvider client={client} messages={ja} appearance={{ theme: "inherit" }}>
  <CoverUpload item={{ kind: "channel", id }} image={cover} onChange={(f) => save(f)} />
  <AvatarUpload item={{ kind: "user", id: userId }} image={avatar} />
  <SlotImage image={avatar} round sizes="40px" className="size-10" />
</UploadUiProvider>
```

`AvatarUpload` / `CoverUpload` are complete form fields. Layouts that draw the
images themselves (a channel header with icon buttons over the cover) use
`SlotEditor`: the same pick → crop → save / re-crop flow with no markup of its
own. Its children draw the image and put triggers where they belong;
`SlotEditMenu` is one trigger (a file picker, or a Change / Edit crop menu once
the path has an upload) and takes a host-styled element via `render`:

```tsx
function Cover() {
  const { has } = useSlotEditor();
  return <SlotImage image={has ? cover : null} />;
}

<SlotEditor item={channel} path="cover" image={cover} onChange={saveCover}>
  <Cover />
  <SlotEditMenu label="Change cover" iconOnly render={<button className="my-overlay-button" />} />
  <SlotEditError />
</SlotEditor>
```

| Component | Props |
| --- | --- |
| `AvatarUpload`, `CoverUpload` | `item`, `path` (default `avatar`/`cover`), `image` (the public preset), `client`, `read` (an editor read; else fetched), `onChange(file \| null)`, `aspect` (default the preset's, else 1:1 / 3:1), `minWidth`, `animation`, `targetWidth` (sharpness warning below it; default 512 / 3000), `accept`, `disabled`, `removable`, `label`, `hint` |
| `SlotEditor` | `item`, `path`, `image`, `client`, `read`, `onChange(file \| null)`, `onError`, `aspect`, `minWidth` (the preset's `Image.MinWidth`), `animation` (`"reject"`), `targetWidth` (default the preset's widest), `round`, `title`, `accept`, `disabled`, `removable`, `children` |
| `useSlotEditor()` | `{ image, crop, has, busy, disabled, error, choose(), pick(file), recrop(), remove() }` inside a `SlotEditor` |
| `SlotEditMenu` | `label`, `iconOnly`, `render` (trigger element; default the kit's outline button), `className`, `align`; the trigger has `data-ckui="slot-edit"` and `data-busy` |
| `SlotEditError` | `className`: the editor's error outside the dialog |
| `ImageCropDialog` | `open`, `onOpenChange`, `source` (`{ url, width, height }` of the oriented upload), `aspect`, `round`, `initialEdit`, `onEditChange`, `onConfirm(edit)`, `targetWidth`, `minWidth` (zoom stops there, a smaller image cannot be confirmed), `busy`, `progress`, `error`, `title` |
| `EncodeProgress` | `progress` (an editor read's `progress`; absent shows "Processing video"), `className`, `appearance` |
| `SlotImage` | `image` (a `PublicImage`; null shows the placeholder), `density`, `round`, `aspect`, `placeholder`, `alt` |
| `VideoPosterPicker` | `open`, `onOpenChange`, `item`, `video` (default `source`), `path` (the poster upload, default `poster`), `image` (its preset), `client`, `read`, `onChange(poster)`, `onError`, `title`, `accept`: frame strip + slider + frame steps over `/frame`, "Use this frame", "Crop…" (in the video's pixels), "Upload image" → `ImageCropDialog` at the video's aspect, "Automatic" |
| `VideoPoster` | `poster` (a `PublicImage` or URL), `aspect` (default the preset's), `density`, `alt`, `children`: full-width, uncropped; never plays |
| `UploadUiProvider` | `client`, `appearance` (`theme`: `light`/`dark`/`auto`/`inherit`, `variables`), `messages` (bundle or list; locales `en de es ja ko zh`), `t` (host translate hook), `density` (default `[2, 3]`), `onError(error, { operation })`, `inlinePreview` (default true) |

Errors are mapped from `UploadError.code` to `errors.*` messages; an unknown
refusal shows the server's message, a fault the generic "try again" line.
Every failure a component shows also goes to its `onError` or the provider's;
aborts never do.

### Renditions

`SlotImage`, `VideoPoster`, `VideoPlayer` and gallery tiles show the narrowest
public width at least their rendered CSS width × density, where density is
`devicePixelRatio` clamped to `[2, 3]`. They re-pick when the box grows, never
step down, and keep the shown image until the wider one has loaded. Set the
range with `UploadUiProvider density`, or per component with `density`.

```tsx
import { RenditionImg, useRendition } from "@openrails/contentkit-upload/ui";
import { pickRendition, publicRenditions } from "@openrails/contentkit-upload";

<RenditionImg outputs={publicRenditions(cover)} alt="" className="size-full object-contain" />;
pickRendition(publicRenditions(cover), 400); // no React
```

### Media gallery and player

`MediaGallery` renders a read (scope it with a `prefix`): each image, each HLS
ladder in `hls` (a video, or audio-only) and each other audio file, as an Instagram-style carousel (swipe,
arrows, ←/→, dots, counter) or a tile grid whose tiles open a lightbox carousel
(Esc closes, focus is trapped and returns to the tile), with a view toggle in
its header. One item renders alone. The carousel spans its column's full width at
the current item's native aspect (uncropped; `maxHeight`, default none, caps it);
only the current slide and its neighbours are mounted, and a video swiped away
pauses. An item's private files are all or nothing: viewers without access
see its public previews (the read's `previews`, a public preset with `First`),
then one locked item with the host's `renderLocked` over the last preview;
locked files carry no URLs.

**Inline preview.** A playable video (a read's `hls`) previews in place: with a
mouse, after 500 ms of hover; on touch, the most visible video in view. It is
the real HLS player, muted, from `previewStart` (e.g. the poster's frame) or 10 % in,
starting at the lowest rendition (ABR then takes over). One plays page-wide,
none while a video plays for real, and leaving unloads the player. Clicking
commits: the same player restarts from 0 with sound and controls. Locked or
unencoded files have no stream, so they never preview. Off with
`UploadUiProvider inlinePreview={false}` or the `inlinePreview` prop
(`MediaGallery`, `VideoPlayer`), and always with reduced motion or Save-Data.
Headless: `useInlinePreview` with `useHlsPlayer`'s `preview(at)`/`unload()`.

```tsx
<MediaGallery
  read={read}                                    // client.read(ref, { prefix: "low-res/" }), or the host's
  hlsBase={(dir) => client.hlsBase(ref, dir)}
  xhrSetup={(xhr) => xhr.setRequestHeader("Authorization", `Bearer ${token()}`)} // same-origin playlists only
  refresh={() => refetchRead()}                  // called shortly before read.expires, and once after a 401/403/404
  poster={posterImage} previewStart={12.5}       // optional: drawn on the first video, whose preview starts there
  renderLocked={({ count }) => <UnlockButton count={count} />}
  renderDetails={(item) => <Downloads item={item} />}
  defaultView="carousel"                         // or view + onViewChange; storageKey remembers the choice
/>
```

`VideoPlayer` (`base`: `client.hlsBase(ref, dir)`, `width`/`height` reserve the box, `poster`, `duration`,
`pending`/`progress` show `EncodeProgress`, `failed`, `layout` `frame`|`fill`,
`maxHeight` default `80svh`, `active`, `xhrSetup`, `refresh`, `expires`, `inlinePreview`,
`previewStart`) loads nothing until previewed or played (hls.js imported then; native HLS on Safari), and never spins
forever: tuned retries surface a dead endpoint within seconds, a watchdog
catches 10 s without progress, and each failure has its own message, a Retry
and a support code: unreachable or blocked (status 0, including missing
`MEDIA_GATEWAY_CORS_ORIGINS`, also logged to the console), no access
(401/403 after one refresh), not found (404 after one refresh: media-gateway
answers an expired or wrong token like a missing object), rate limited (429, from the
media host's ingress: shown, never retried in a loop), unsupported in
this browser. A read answers the same token until `expires`, so nothing needs
re-reading before then: with `refresh`, the gallery and the player read again
one minute before it (`useRefreshBeforeExpiry`, `expiryDelay`), and a player
given the new `expires` loads its playlists again at the playhead when their
URLs carry the token. The refresh after a 401/403/404 stays as the fallback. Headless: `useHlsPlayer`, `useCarousel` and `useGalleryView` in
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

Wire types (`src/wire.gen.ts`) are generated from the Go handlers:
`go test ./media/internal/wirets -update`. Integration tests and the browser
specs run against `media/internal/uploadtestserver` (the real handlers over
MinIO, with a stand-in worker).

```sh
pnpm test               # unit + jsdom hooks/components
CONTENTKIT_TEST_S3_ENDPOINT=http://localhost:9000 CONTENTKIT_TEST_S3_ACCESS_KEY=… \
CONTENTKIT_TEST_S3_SECRET_KEY=… pnpm test:integration   # real handler + MinIO
pnpm build && pnpm screenshots   # demo/ in Chromium, light/dark × desktop/mobile (SCREENSHOT_DIR)
```

UI primitives come from `pnpm dlx shadcn@4.21.0 add …` (`components.json`); local
edits are marked `// Local:`.

The `sdk-release` workflow packs `dist` (`pnpm pack`) onto every published `v*` release, versioned by the tag.

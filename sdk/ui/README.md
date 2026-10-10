# @openrails/contentkit-ui

ContentKit in the browser: a framework-free client, content URLs, React hooks
and styled components (uploads with cropping, video posters, galleries, HLS
playback, comments, reactions, favorites, polls and their staff screens).
`@openrails/contentkit-ui@X.Y.Z` speaks ContentKit `vX.Y.Z`.

```sh
pnpm add @openrails/contentkit-ui@X.Y.Z
```

| Import | Contents | Needs |
| --- | --- | --- |
| `@openrails/contentkit-ui` | styled components, `ContentKitUiProvider`, i18n | `react`, `react-dom` |
| `@openrails/contentkit-ui/react` | `ContentKitProvider` and every hook | `react` |
| `@openrails/contentkit-ui/client` | `createContentKitClient`, `ContentKitError`, generated wire types, crop/aspect/rendition/ABR/gallery math | nothing |
| `@openrails/contentkit-ui/urls` | content codes, `[/{lang}]/{route}/{CODE}[/{slug}]` paths, `canonical` | nothing |
| `@openrails/contentkit-ui/locales/{de,es,ja,ko,zh}` | message bundles (English is built in) | |
| `@openrails/contentkit-ui/styles.css` | the stylesheet, scoped under `.ckui` (the root entry also installs it) | |

`react` and `react-dom` are optional peers, so `/client` and `/urls` work
without React. hls.js loads on first play.

## Client and providers

```tsx
import { createContentKitClient } from "@openrails/contentkit-ui/client";
import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import { ContentKitUiProvider } from "@openrails/contentkit-ui";
import { createContentURLs } from "@openrails/contentkit-ui/urls";

const contentkit = createContentKitClient({
  baseUrl: "/api/v1/contentkit",            // where the host mounts ContentKit
  mounts: { upload: "/api/v1/user-uploads" }, // optional: modules mounted elsewhere (a string, or (ref) => string)
  fetch: auth.authFetch,                    // an authenticating transport
  token: () => auth.getAccessToken(),       // HLS playlists on the API origin (hls.js uses XHR)
  language: () => lang,                     // Accept-Language
});
const urls = createContentURLs({ routes: { video: "watch", gallery: "g" }, languages: ["en", "es"], origin: "https://example.com" });

<ContentKitProvider
  client={contentkit}
  viewer={user?.id ?? null}                 // the signed-in user as ContentKit sees it
  onSignIn={() => openSignIn()}             // signed-out visitors are asked instead of acting anonymously
  urls={urls}
  navigate={(to, o) => navigate(to, o)}
  onChange={(change) => queryClient.invalidateQueries({ queryKey: ["media", change.ref.id] })}
  onError={(error, { operation }) => report(operation, error)}
>
  <ContentKitUiProvider appearance={{ theme: "inherit" }} language={lang} density={[2, 3]}>
    <App />
  </ContentKitUiProvider>
</ContentKitProvider>;
```

- **Mounts.** Without `mounts`, modules live under `baseUrl`: content at the
  root, `/media` (reads, HLS), `/media/upload`, `/codes`, `/taxonomy`.
- **`client.media`**: `upload`, `put` (upload, commit, wait), `commit`, `read`,
  `waitFor`, `editorView`, `getFrame`, `hlsBase(ref, dir)`, `discard`, and
  `xhrSetup` for players (`<MediaGallery xhrSetup={contentkit.media.xhrSetup} />`).
  Uploads hash in a Web Worker, send one checksum-bound PUT up to 64 MiB and
  resumable multipart above (`onState`/`resume`). The bucket must allow CORS
  `PUT` from the app's origin with `Content-Type` and `x-amz-checksum-sha256`.
- **Content modules**, one per resource, each method one route of the
  generated route table with its generated types: `posts`, `comments`,
  `reactions`, `favorites`, `polls`, `bans` (`"owner"` or `"global"`),
  `moderation`, `taxonomy`, `codes`. Lists take `limit`/`offset` (the review
  queue a `cursor`); staff lists are `posts.adminList`, `polls.adminList`,
  `comments.adminList`.
- **Images of posts and polls** go to the item's server-named upload path:
  `media.uploadNamed(ref, file)` resolves the name content routes take;
  `media.uploadInline(postId, file)` resolves a body image's URL;
  `posts.uploadCover`, `polls.uploadImage` and `polls.uploadOptionImage` do
  both steps. `folders` renames the post and poll media kinds.
- **`client.subscribe(listener)`** receives every successful mutation
  (`media.committed`, `media.processed`, `comment.created`, `reaction.changed`,
  `poll.updated`, `ban.saved`, …); `ContentKitProvider onChange` is the same
  stream, for the host's cache.
- **`ContentKitProvider`** holds the client, URL config, `navigate`,
  `onChange`, `onError` and the read store hooks share: one request per item
  and options while any hook shows it; `set()` and processed uploads update
  every reader in place.
- **`ContentKitUiProvider`**: `appearance` (`theme`
  `light`/`dark`/`auto`/`inherit`, `variables` as `--ckui-*`), `language`
  (loads its built-in bundle), `messages` (bundles over it), `t` (host
  translate hook), `density` (default `[2, 3]`), `inlinePreview` (default true).

## Hooks

Hooks read the client from `ContentKitProvider`; a `client` option overrides it.

```tsx
const q = useUploadQueue({ ref, path: "originals/{name}" });
q.add(input.files!); q.move(id, 0); await q.commit();

const one = useUpload();
await one.upload(file, { ref, path: "inline/x.png", put: {} });

const img = useSlotImage({ ref, path: "avatar", image });
const crop = useSlotCrop({ ref, path: "avatar", file: img.file, aspect: "1:1", onSaved: img.set });
```

Content hooks share one store per client: one request per item while any
hook shows it, kept per `viewer`, with every change made through the client
applied in place. Reactions, favorites and votes apply at once and roll back
if the server refuses them.

```tsx
const thread = useComments(ref, { sort: "best" });
await thread.post("Nice!", { replyTo: parentId });
await thread.react(comment, 1);
const { counts, toggle } = useReaction(ref);
const { favorited, toggle: fav } = useFavorite(ref);
const { poll, vote } = usePoll(null, { language }); // the newest live poll
const editor = usePollEditor(pollId);                // staff
```

Also `useCommentReplies`, `useCanComment` (may the caller comment, the ban
that stops it, and what it may do to others' comments), `useLatestComments`,
`usePolls`, `usePosts`, `usePost`, and for staff `useAdminComments`,
`useModerationQueue`, `useCommentBans`.

Also: `useCrop`, `useVideoImages`, `useFrameStrip`, `useVideoFrame`,
`useVideoPoster`, `useEncodeProgress`, `useHlsPlayer`, `useCarousel`,
`useGalleryView`, `useInlinePreview`, `useRefreshBeforeExpiry`, `usePresets`,
`useContentKitClient`, `useContentURLs`, `useErrorReporter`.

### useMediaRead

```tsx
const { read, loading, error, processing, reload, refresh, set } =
  useMediaRead(ref, { prefix: "low-res/", editor: true, window: { start: 0, end: 120 } });
```

One read per item and options, shared by every hook showing it. It reads
again shortly before the URLs expire, waits out `rate_limited`, polls while
uploads process (editor reads, every 2.5 s; `poll`), and reloads after any
commit through the same client. `window` splits a long item into reads of
`chunk` files (default 50) merged into one result; windows once read stay
loaded. `refresh()` joins a read in flight (a player's grant refresh);
`reload()` restarts it. `read` shows a host's read until `reload()`.

### useMediaFolder

```tsx
const folder = useMediaFolder(ref, { paths: ["images/{name}", "videos/{name}"], commit: "manual" });
folder.add(files);               // screened, queued; returns the refusals
await folder.commit();           // the uploaded files, in queue order
await folder.move(path, 0); await folder.rename(path, "cover");
await folder.edit(path, edit); await folder.replace(path, file); await folder.remove([path]);
folder.discard();                // stop uploads and empty the queue
```

An item's folder for its editors: the editor read (all windows), an upload
queue, and the kind's upload rules from the editor read, which screen type,
size and file caps before anything uploads and name files around names
taken. `groups` lists each path's uploads; `commit: "auto"` commits each
upload once those before it have (a draft). Failures go to the provider's
`onError` with an operation (`upload`, `folder.commit`, `folder.update`,
`folder.process` for uploads the worker fails after the editor opened) and
the file's name.

### useEditorCrop

```tsx
const crop = useEditorCrop(ref, cropping); // a path, or null
// crop.status: idle | loading | cropping (source, edit) | saving | done | error
await crop.save(edit);
```

Re-crops a kept upload: while the path is set, its editor view (the
original, oriented, unedited) and current edit load; `save` commits an edit
op. Nothing is uploaded.

### usePublicImage

```tsx
const { image, rule, isDefault } = usePublicImage("user", userId, "avatar");
<SlotImage image={image} fallback={<Initials name={name} />} />
```

An item's image for a public preset: the exact renditions from a read's
`public`, or from a host listing passed as `image` (no read), else the
kind's default image. Published files carry a generation, so a URL built
from a preset template only ever names the default; `rule` (from
`GET /media/presets`) gives the shape, widths and narrowest edit.

### useCanonicalContent

```tsx
useCanonicalContent(video.link, { title: video.title, image: posterURL, location: pathname + search, defaultLanguage: "en" });
```

Once a content page's link arrives, replaces the address with its canonical
path (code spelling, merged code, current slug) through the provider's
`navigate`, else `history.replaceState`, and keeps `<link rel=canonical>`,
`og:url`, `og:title`, `og:image` and hreflang alternates (each language's
own slug, plus `x-default`) in the head while mounted. Needs the provider's
`urls`; `/urls` also exports `hreflang()` and `canonicalURL()` (drops
tracking parameters) for server rendering.

### useNearViewport

```tsx
const [ref, near] = useNearViewport({ margin: "800px 0px", leaveMargin: "2400px 0px" });
```

Whether an element is near the viewport, to mount media as it approaches;
with `leaveMargin`, false again far away.

## Components

`AvatarUpload`, `CoverUpload` (complete form fields); `SlotEditor` with
`SlotEditMenu`, `SlotEditError` and `useSlotEditor()` (the same pick, crop,
save and re-crop flow inside a host layout); `SlotImage`; `ImageCropDialog`;
`VideoPosterPicker`, `VideoPoster`; `EncodeProgress`; `RenditionImg`;
`MediaGallery`, `VideoPlayer` and `VideoMiniPlayer` (HLS with ABR, inline
muted previews, a grant refresh before `expires`, and a reason, Retry and
support code for every failure; see below). A `PublicPreset` (`{ preset, aspect, renditions }`)
is what a slot or poster shows; a read's `public` lists `PublicImage`s.

### MediaFolderEditor

```tsx
<MediaFolderEditor item={post} paths={["images/{name}", "videos/{name}"]} commit="auto" ref={handle}
  toolbar={<ZipImport />} rowActions={(f) => <SetCover file={f} />} footer={note} />
handle.current.discard();
```

`useMediaFolder` with its UI: a drop zone stating the rules, a queue with
progress, retry and remove, and each path's uploads as a sortable list with
thumbnail, name, edited badge, encode progress and failure; crop (with
`aspects` when the path has no preset shape), rename, replace and bulk
remove. `commit="manual"` adds the queue with "Add N"; `"auto"` commits as
files finish. `confirmRemove` replaces the browser's confirm.

### SortableList

```tsx
<SortableList items={rows} id={(r) => r.id} name={(r) => r.title} onMove={(from, to) => move(from, to)} layout="grid">
  {(row, handle) => <>{handle}<Row row={row} /></>}
</SortableList>
```

Reorders by a drag handle with a mouse, a finger or the keyboard (Space or
Enter lifts, arrows move, Space or Enter drops, Escape cancels), with
localized screen reader announcements. `onMove` fires once, on drop.

### ImageCropDialog shapes

`aspects={["", "1:1", "4:5", "16:9"]}` adds a shape chooser (`""` is the
original's, turning with the image); a preview of the result and its size
sit under the controls (`preview={false}` hides them), and Reset returns to
the starting shape and edit.

### MediaGallery item

`<MediaGallery item={post} prefix="low-res/" renderLocked={unlock} />` reads
through the client: the read (refreshed, reloaded after commits), HLS bases,
playlist auth and the grant refresh. Passing `read`, `hlsBase`, `xhrSetup`
and `refresh` yourself still works. A locked item's public teaser carries
the locked count and `renderLocked`, so the unlock is on the first slide;
the stage is never taller than `maxHeight` (default `80svh`).

### VideoPosterPicker frames from another video

`frames={{ item: otherVideo, path: "source" }}` browses another item's video;
the chosen frame (cropped or whole) is uploaded to the poster path as an
image. Which video to offer is the host's choice.

### HoverPreview

```tsx
<div className="card relative"><Thumbnail /><HoverPreview item={video} duration={video.duration} /></div>
```

A card's muted inline preview over the client's HLS base, fading in on hover
(or while it is the most visible card on touch screens), one page-wide, off
with reduced motion or Save-Data.

### Images: fallback, skeleton, fit and LazyMount

`RenditionImg`, `SlotImage` and `VideoPoster` take `fallback` (shown without
renditions or after a failed load, e.g. a 404), and `RenditionImg` and
`SlotImage` take `skeleton` (a pulsing fill until loaded) and `fit`
(`cover` or `contain`). `<LazyMount placeholder={box}>` mounts its children
as it nears the viewport (`useNearViewport` options).

### MediaReadinessNotice

`<MediaReadinessNotice item={post} description="Only editors see it until then." />`
renders nothing once every upload is processed; while some process, a
notice with the first video's encode progress; when uploads failed, which
ones.

## Video player

`VideoPlayer` plays a ladder's HLS folder (`client.media.hlsBase(ref, dir)`)
with its own controls: seek bar with sprite preview, volume, subtitles,
audio, quality (Auto or a fixed rung), speed, picture in picture, theater,
fullscreen and a mini player.

```tsx
<VideoPlayer
  base={contentkit.media.hlsBase(ref, "hls/")}
  xhrSetup={contentkit.media.xhrSetup}
  refresh={reload} expires={read.expires}       // re-grant before the token expires
  width={video.w} height={video.h} duration={video.dur} poster={posterPreset}
  autoPlay startAt={resumePoint}                 // resume; never a muted autoplay
  keyboard="global"                              // a watch page; default "focus"
  audioLanguage={lang} subtitleLanguage={null}   // until the viewer chooses
  onProgress={(p) => saveResume(p.time, p.played)} // interval, pause, seek, end, page hide, unload
  onEvent={(e) => analytics(e)}                  // play, pause, seek, quality, audio, subtitles, fullscreen, error…
  theater={theater} onTheaterChange={setTheater}
  onMiniPlayer={setHandoff}
  renderDownloads={({ className, container }) => <Downloads className={className} container={container} />}
  menuItems={[{ label: "Version", value, options, onChange }, { label: "Report a problem", onSelect }]}
/>
{handoff && <VideoMiniPlayer handoff={handoff} xhrSetup={contentkit.media.xhrSetup} onExpand={openWatchPage} onClose={() => setHandoff(null)} />}
```

- **Shortcuts:** Space/K play, ←/→ 5 s, J/L 10 s, ↑/↓ volume, M mute, C
  subtitles, F fullscreen, T theater, I mini player, 0–9 a tenth of the way,
  Home/End, `<`/`>` speed. Never while typing or in a menu.
- **Touch:** a tap shows or hides the controls; a double tap on either side
  seeks 10 s, and further taps keep seeking. A double click is fullscreen.
- **Remembered per browser:** volume and mute (`ckui.player.volume`), the
  audio and subtitle languages (`ckui.player.tracks`) and a fixed quality
  (`ckui.player.quality`); each `*Key` option renames or disables one.
- **Mini player:** `onMiniPlayer` gets a `PlayerHandoff` (position, playing,
  languages, size, poster). `VideoMiniPlayer` docks it in a corner and calls
  `onExpand({ time, playing })` or `onClose({ time })`; where it lives and
  what expand opens are the host's.
- **Theming:** `appearance.variables.playerAccent` colors the played range
  and pressed toggles (default white). Menus are always dark.

## Comments, reactions and polls

`Comments` (threads with one-level replies, tombstones, held and rejected
states shown to their author, reactions, edit, delete, ban and rate-limit
notices), `ReactionButtons`, `FavoriteButton` (moves a host-given `count`)
and `Poll` (a final vote with results scaled to the leading option, or a
free-text answer and its groups; `results="always"` shows results before
voting). With an `onSignIn` (the provider's or their own), signed-out
visitors are asked to sign in; without one they comment under a name and
react and vote anonymously, as ContentKit allows.

Staff: `CommentModeration` (every comment, the review queue, the site's
bans), `CommentBans` with `CommentBanDialog`, and `PollEditor`. They show what
the caller's permissions allow and say so when a route refuses; the host
decides who sees them.

```tsx
<Comments item={{ kind: "video", id }} count={video.comment_count} userHref={(u) => `/u/${u.username}`} />
<Poll language={lang} />
<CommentModeration contentKinds={["video", "post"]} itemHref={(i) => `/${i.kind}/${i.id}`} />
```

## Errors

Every call rejects with `ContentKitError`: `code` (the server's, or `network`,
`storage`, `aborted`, `resume_mismatch`, `decode`, `render_timeout`),
`status`, `retryAfter`, `details` (image and video limits), `blobs`
(`not_uploaded`), `ban` (`comment_banned`) and `action` (`rate_limited`),
plus `refusal`, `isLimit`, `isCeiling`, `blocksQueue` and `transient`. Every
code has a message in every bundle (`useMessages().error(e)`).

## Migrating from the two old packages

| Before | Now |
| --- | --- |
| `@openrails/contentkit-upload` | `@openrails/contentkit-ui/client` |
| `@openrails/contentkit-upload/react` | `@openrails/contentkit-ui/react` |
| `@openrails/contentkit-upload/ui` | `@openrails/contentkit-ui` |
| `@openrails/contentkit-upload/locales/*`, `/styles.css` | `@openrails/contentkit-ui/locales/*`, `/styles.css` |
| `@openrails/contentkit-urls` | `@openrails/contentkit-ui/urls` |
| `createUploadClient({ endpoint, readEndpoint })` | `createContentKitClient({ baseUrl, mounts: { upload, media } }).media` |
| `UploadClient`, `UploadApi` | `MediaClient`, `MediaApi` |
| `UploadUiProvider client onError …` | `ContentKitProvider client onError` + `ContentKitUiProvider appearance messages t density inlinePreview` |
| `UploadError`, `UploadErrorCode` | `ContentKitError`, `ContentKitErrorCode` |
| `useUploadQueue(client, o)`, `useUpload(client)` | `useUploadQueue(o)`, `useUpload()` |
| `useRead(client, ref, o)` | `useMediaRead(ref, o)` |
| `useSlotImage(client, o)`, `useSlotCrop(client, o)` | `useSlotImage(o)`, `useSlotCrop(o)` |
| `useVideoImages`, `useFrameStrip`, `useVideoFrame`, `useVideoPoster` `(client, o)` | the same `(o)` |
| `PublicImage` (preset at an item) | `PublicPreset` |
| `SlotImage`, `VideoPoster` `placeholder` | `fallback` |
| `UploadUi*` types, `UploadUiRoot` | `ContentKitUi*`, `ContentKitUiRoot` |

## Development

```sh
pnpm check                # typecheck, lint, unit + jsdom tests, build
CONTENTKIT_TEST_URL=… CONTENTKIT_TEST_S3_ENDPOINT=… CONTENTKIT_TEST_S3_ACCESS_KEY=… CONTENTKIT_TEST_S3_SECRET_KEY=… \
  pnpm test:integration   # the real handlers (contentkit.Runtime.Handler) over PostgreSQL and MinIO
pnpm build && pnpm screenshots   # demo/ in Chromium, light/dark × desktop/mobile
                                 # (social.html needs the integration variables)
```

The wire types, route table and error codes in `src/client/generated/` are
generated from ContentKit's route catalog (`pnpm contract`; CI runs
`pnpm contract:check`). UI primitives come from
`pnpm dlx shadcn@4.21.0 add …` (`components.json`); local edits are marked
`// Local:`. The `release` workflow publishes the package to npm for every
published `v*` release.

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

const { read, reload, set } = useRead(ref, { prefix: "low-res/", editor: true });
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
`useGalleryView`, `useInlinePreview`, `useRefreshBeforeExpiry`,
`useContentKitClient`, `useContentURLs`, `useErrorReporter`.

## Components

`AvatarUpload`, `CoverUpload` (complete form fields); `SlotEditor` with
`SlotEditMenu`, `SlotEditError` and `useSlotEditor()` (the same pick, crop,
save and re-crop flow inside a host layout); `SlotImage`; `ImageCropDialog`;
`VideoPosterPicker`, `VideoPoster`; `EncodeProgress`; `RenditionImg`;
`MediaGallery`, `VideoPlayer` and `VideoMiniPlayer` (HLS with ABR, inline
muted previews, a grant refresh before `expires`, and a reason, Retry and
support code for every failure; see below). A `PublicPreset` (`{ preset, aspect, renditions }`)
is what a slot or poster shows; a read's `public` lists `PublicImage`s.

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
| `useRead(client, ref, o)` | `useRead(ref, o)` |
| `useSlotImage(client, o)`, `useSlotCrop(client, o)` | `useSlotImage(o)`, `useSlotCrop(o)` |
| `useVideoImages`, `useFrameStrip`, `useVideoFrame`, `useVideoPoster` `(client, o)` | the same `(o)` |
| `PublicImage` (preset at an item) | `PublicPreset` |
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

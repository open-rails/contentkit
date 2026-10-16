# @openrails/contentkit-ui

ContentKit in the browser: a framework-free client, content URLs, React hooks
and styled components (uploads with cropping, video posters, galleries, HLS
playback, comments, reactions, favorites, polls and their staff screens).
UI releases share the backend's major/minor API version; UI-only patches can
advance independently (for example, UI `0.89.1` with ContentKit `v0.89.0`).

```sh
pnpm add @openrails/contentkit-ui@X.Y.Z
```

| Import | Contents | Needs |
| --- | --- | --- |
| `@openrails/contentkit-ui` | styled components, `ContentKitUiProvider`, i18n | `react`, `react-dom` |
| `@openrails/contentkit-ui/react` | `ContentKitProvider` and every hook | `react` |
| `@openrails/contentkit-ui/client` | `createContentKitClient`, `ContentKitError`, generated wire types, crop/aspect/rendition/ABR/gallery math | nothing |
| `@openrails/contentkit-ui/urls` | content codes, `[/{lang}]/{route}/{CODE}[/{slug}]` paths, `canonical` | nothing |
| `@openrails/contentkit-ui/editor` | `RichTextEditor`, `richTextEditor` (ContentManager's body editor) | the optional peers `@tiptap/core`, `@tiptap/pm`, `@tiptap/react`, `@tiptap/starter-kit`, `@tiptap/extension-image`, `@tiptap/extensions` (3.31+) |
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
  baseUrl: "/api/contentkit",               // the host's contentkit.Routes.Prefix
  mounts: { upload: "/api/v1/user-uploads" }, // optional: modules mounted elsewhere (a string, or (ref) => string)
  fetch: auth.authFetch,                    // an authenticating transport
  token: () => auth.getAccessToken(),       // HLS playlists on the API origin (hls.js uses XHR)
  language: () => lang,                     // Accept-Language
});
const urls = createContentURLs({ routes: { video: "watch", gallery: "g" }, languages: ["en", "es"], origin: "https://example.com" });

<ContentKitProvider
  client={contentkit}
  viewer={user?.id ?? null}                 // the signed-in user as ContentKit sees it
  language={lang}                          // same value as the client's language callback
  accessRevision={accessRevision}          // increment when permissions/entitlements change
  onSignIn={() => openSignIn()}             // asks signed-out visitors to sign in where the server refuses them
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

Keep `viewer`, `language` and `accessRevision` current on the provider. A change
starts fresh media and social reads without remounting the children. Old requests
are abandoned when their last reader leaves; their results cannot fill the new
scope. `accessRevision` is optional when the host has no changing access state.
It is a cache signal, not an authorization decision. Without a provider, replace
the client when its session or request language changes. Host-supplied `read` and
`initial` data must also belong to the current viewer and language.

Writes retain the cache scope they started in. After a scope change, a late
notification refetches affected reads instead of copying its response into them;
`onChange` still runs. Use a separate client for each independent session or
request-language context, not one client across differently scoped providers.
The framework-free client exposes `setReadScope(key)` and passes that key as
the second `subscribe` / `onChange` argument, or `null` when the scope changed during the
operation. This does not change the operation's result or its authentication.

- **Mounts.** Without `mounts`, modules live under `{baseUrl}/v1`: content,
  entities and the content model at its root, `/media` (reads, HLS),
  `/media/upload`, `/codes`. A mount is a module's full base URL.
- **`client.media`**: `upload`, `put` (upload, commit, wait), `commit`, `read`,
  `waitFor`, `editorView`, `getFrame`, `hlsBase(ref, dir)`, `discard`, and
  `hls` for players (`<VideoPlayer loader={contentkit.media.hls} />`): playlists
  load through the client's fetch, so they carry what its calls carry (an
  `authFetch` adds DPoP proofs); segments carry only cookies.
  Uploads hash in a Web Worker, send one checksum-bound PUT up to 64 MiB and
  resumable multipart above (`onState`/`resume`). The bucket must allow CORS
  `PUT` from the app's origin with `Content-Type` and `x-amz-checksum-sha256`.
- **Content modules**, one per resource, each method one route of the
  generated route table with its generated types: `contents`, `catalog`, `comments`,
  `reactions`, `favorites`, `polls`, `bans` (`"owner"` or `"global"`; `get("global", user)`),
  `moderation`, `entities`, `codes`. Lists take `limit`/`offset` (the review
  queue and group listings a `cursor`); `contents.manage` lists what the caller
  manages (`kind`, `state` incl. `deleted` for `contents.restore`, `q`); `catalog.totals({kinds, languages})`
  counts what is open; staff lists are `polls.adminList` (`state`, `q`),
  `comments.adminList` (`contentKind`, `author`); `entities.members(id)` pages
  an entity's memberships, `entities.ofContent(id)` / `setOfContent` a content's,
  `entities.uploadImage(entity, "cover", file)` its cover; `client.kinds()` the content model.
  `contents.manage({kind: "gallery"})` uses `/manage/gallery`; omitting `kind`
  lists manageable content across kinds at `/manage`.
- **Images of content and polls** go to the item's server-named upload path:
  `media.uploadNamed(ref, file)` resolves the name content routes take
  (`contents.setCover(id, version, name)`: every file of a content lives in a
  version); `media.uploadInline({kind, id, version}, file)` puts a body image
  in the version's private files, copied into the other versions, and
  resolves its reference (`ref`, `contentkit:i-{uuid}`, what the body stores:
  `imageRef(name)`) and a URL that shows it now; `polls.uploadImage` and
  `polls.uploadOptionImage` do both steps. Reads resolve references: a
  content's `body` shows its images' signed URLs (the opened version's
  private files) and `images` maps each name to its URL. `folders` renames
  the poll media kind. Public image files exist only while the item is live;
  `useContent` shows an unpublished item's images to its editors (below).
- **`client.config()`** says what the content module allows: which
  interactions signed-out visitors may make (`content.Options.Anonymous`,
  none by default) and the longest comment (`CommentMaxLength`, 2,200 by
  default; longer is `comment_too_long` with `details.max`).
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
const { favorited, count, toggle: fav } = useFavorite(ref);
const { poll, vote } = usePoll(null, { language }); // the newest live poll
const editor = usePollEditor(pollId);                // staff
```

A content editor puts `text.body` in its rich-text field and inserts what
`uploadImage(file)` resolves: URLs that show the images now (the file for a
fresh upload, public files, signed editor views of an unpublished item's
images, read and refreshed as needed). `putText({ title, body })` stores each
of those URLs as the image's reference (`storedBody(html)` does the same for
a host's own save); `coverSrc` shows the cover, `imageSrc(name)` any image.

```tsx
const editor = useContent(id, "blog", "en");
<RichText value={editor.text?.body} onImage={editor.uploadImage} onSave={(html) => editor.putText({ title, body: html })} />
<img src={editor.coverSrc} alt="" />
```

Also `useContentConfig` (`client.config()`, read once), `useCommentReplies`,
`useCanComment` (may the caller comment, may signed-out visitors, the ban
that stops it, and what it may do to others' comments), `useLatestComments`,
`usePolls`, `useManagedContents` (`kind`, `state`, `q`), `useContent` (with `publish`, `restore`), and for staff `useAdminComments`
(`contentKind`, `author`), `useModerationQueue`, `useCommentBans`, `useGlobalCommentBan` (one user's), and
`useStaffAccess`, `useAdminLocation`, `useFlowClient` and `usePaged`, which the admin flows are built from.

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
folder.add(files);               // screened and queued; refusals in folder.refused
await folder.commit();           // the uploaded files, in queue order
await folder.move(path, 0); await folder.rename(path, "cover");
await folder.edit(path, edit); await folder.replace(path, file); await folder.remove([path]);
folder.discard();                // stop uploads and empty the queue
```

An item's folder for its editors: the editor read (all windows), an upload
queue, and the kind's upload rules from the editor read, which screen type,
size and file caps before anything uploads and name files around names
taken. `groups` lists each path's uploads; `commit: "auto"` commits each
upload once those before it have (a draft). Files added before the editor
read arrives wait in `waiting` and are screened when its rules do (or
refused if the read fails or states no rules). `ref` may be null until the item exists,
so a composer can create its draft on the first drop. `busy` is true while
work is in flight (files waiting, uploads queued or running, a commit or
update); `fileCount` counts the uploads and the files on their way in. A
committed file leaves the queue once the read lists it. Failures go to the provider's
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
const { image, rule, isDefault } = usePublicImage({ kind: "user", id: userId }, "avatar");
<SlotImage image={image} fallback={<Initials name={name} />} />
```

An item's image for a public preset: the exact renditions from a read's
`public`, or from a host listing passed as `image` (no read), else the
kind's default image when the preset has one (`rule.default`), else none.
Published files carry a generation, so a URL built from a preset template
only ever names the default; `rule` (from `GET /media/presets`) gives the
shape, widths and narrowest edit.

### useCanonicalContent

```tsx
useCanonicalContent(video.link, { title: video.title, image: posterURL, location: pathname + search });
```

Once a content page's link arrives, replaces the address with its canonical
path (language prefix, the reader's when the address has none; code
spelling, merged code, current slug; the query, `?v=` included, kept) through
the provider's `navigate`, else `history.replaceState`, and keeps `<link rel=canonical>`,
`og:url`, `og:title`, `og:image` and hreflang alternates (each language's
own slug, plus `x-default`, the default language's) in the head while
mounted. As on the server, every path carries its language, the default
included, and a version's (`?v=`) canonical URL is its language page. Unmounting removes
them, server-rendered ones included, so the next page sets its own. Needs the provider's
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
is what a slot or poster shows; a read's `public` lists `PublicImage`s. A
slot crops at its image's aspect, else its public preset's
(`GET /media/presets`), so an item without an image yet still crops a 3:1
cover at 3:1.

### Card lists: decorations and locked cards

Every card list (`ContentGrid`, `ContentListing`, `ContentRail`,
`SearchResults`, `ContentHistory`) and `ContentCard` take `decorate(card)`:
the host's badges, drawn over the cover (place them yourself; clicks reach
the card, and click signals stay intact). Cards carry what badges need:
`access.level` and `access.tiers` (the effective rule, the same for every
reader), `version.has_audio`, `viewer.favorited` and `entities`.

`ContentGrid` and `ContentListing` take `locked`: `show` (default) marks the
cards the reader doesn't open, `hide` leaves them out, and `collapse` lists
the open ones and puts the rest behind a counted toggle that loads them on
opening (`useLockedCount` counts them).

### MediaFolderEditor

```tsx
<MediaFolderEditor item={post} paths={["images/{name}", "videos/{name}"]} commit="auto" ref={handle}
  toolbar={<ZipImport />} rowActions={(f) => <SetCover file={f} />} footer={note} />
handle.current.discard();

// Or the host owns the folder and renders with its state:
const folder = useMediaFolder(draft, { paths, commit: "auto" }); // draft: null until created
<MediaFolderEditor folder={folder} />
<Button disabled={folder.busy || (!title && !text && !folder.fileCount)}>Publish</Button>
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
playlist auth and the grant refresh. Passing `read`, `hlsBase`, `loader`
and `refresh` yourself still works. A locked read lists no files, so the
lock is uncounted; its public teaser carries `renderLocked`, so the unlock is
on the first slide;
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
  loader={contentkit.media.hls}
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
{handoff && <VideoMiniPlayer handoff={handoff} loader={contentkit.media.hls} onExpand={openWatchPage} onClose={() => setHandoff(null)} />}
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
notices, the server's longest comment), `ReactionButtons`, `FavoriteButton` (with the server's count)
and `Poll` (a final vote with results scaled to the leading option, or a
free-text answer and its groups; `results="always"` shows results before
voting). Signed-out visitors comment under a name, react and vote where the
server allows it (`content.Options.Anonymous`, read from the comment standing
and `client.config()`); elsewhere they are asked to sign in (`onSignIn`, the
provider's or their own) or, without one, told to. On locked content they
follow its kind's locked view (`kinds.Kind.Locked`): hidden comments show an
unlock note without a count, hidden reactions render nothing, a hidden
favorite count is left out, and cards drop hidden counts, never showing 0.

Staff building blocks: `CommentBans` (paged) with `CommentBanDialog`, and
`PollEditor` (deleting asks for a typed confirmation). The complete staff screens are the admin flows below.

```tsx
<Comments item={{ kind: "video", id }} count={video.comment_count} userHref={(u) => `/u/${u.username}`} />
<Poll language={lang} />
```

## Groups

Tags, artists, series, characters, channels and rails: the kinds the site's
content model declares. `useKinds()` reads it (`GET /v1/kinds`); until that
route is served, pass the host's model as `initial` (and `kinds` to the
components below).

```tsx
const { items, hasMore, loadMore } = useGroups({ kind: "artist", letter: "a", sort: "count" }); // parent, scope, contentKind, language
const g = useGroup(id);           // g.group: names, text, attributes, parent, scope, children, edges, counts
await g.update({ attributes: { artist_type: "circle" } }); // staff: create, names, edges, merge, remove, restore
await g.uploadCover(file);         // into the group kind's folder (cover/{name}), then selected
const members = useGroupMembers(id);       // add, remove, reorder
const content = useContentGroups(contentId); // content.set(memberships, kinds)
```

- **`GroupDirectory`**: one kind's groups with A–Z buckets (`letter` and
  `onLetterChange` to keep it in the URL), content counts, `layout` list or
  grid, paged. **`GroupHeader`**: a group page's cover, names, description,
  typed attributes, parent, scope and sub-groups. Links go to each group's
  canonical path (`href` overrides) through the provider's `navigate`.
- **`GroupEditor`** (staff): create, or edit slug, parent, scope, declared
  attributes, names and aliases per language, description and sort key per
  language, cover, related groups; `GroupMergeDialog`; delete and restore.
- **`MembershipEditor`** (staff): a content's groups per group kind, with the
  version (`versions`), role, position and window each kind allows; one save.
  **`GroupMembers`**: a group's members, reordered, windowed, used as cover.
- `labels` names kinds, attributes, roles and values by identifier; others
  show humanized. Covers render `cover.renditions`; `renderCover` stands in
  until group reads carry them.

## Reference administration workbench

`ContentAdmin` is an optional library component composing the existing content,
group, moderation, poll and statistics managers. It is not a standalone server
portal, a sign-in page or a SaaS control plane. A host can continue to compose
any individual manager in its own administration area.

```tsx
import { ContentAdmin } from "@openrails/contentkit-ui";
import { richTextEditor } from "@openrails/contentkit-ui/editor";
import "@openrails/contentkit-ui/styles.css";

<ContentAdmin
  client={contentkit}
  basePath="/admin/content"
  navigation={{ pathname, search, navigate }}
  appearance={{ theme: "inherit" }}
  sections={["contents", "groups", "moderation", "polls", "statistics"]}
  contentKinds={["article", "video"]}
  labels={{ article: "Articles", video: "Videos" }}
  content={{ renderEditor: richTextEditor }}
/>;
```

The client is an existing ContentKit client, or pass `baseUrl` and the host's
`auth` transport. `guard`, `language`, `messages` and `onError` follow the
individual managers' contracts below. Without `navigation`, the workbench uses
browser history; without `basePath` as well, navigation stays in memory.

`GET /kinds` supplies languages and configured content and group kinds. Omit
`contentKinds` to offer all content kinds; omit `groupKinds` to offer the
site-owned group kinds. Explicit lists restrict and order those choices, and
unknown kinds are omitted. The routes beneath `basePath` are
`contents/<kind>[/new|/<id>]`, `groups/<kind>[/new|/<id>]`, `moderation`,
`polls[/new|/<id>]` and `statistics`; each manager retains its own filters and
subviews. Unknown or excluded routes show an unavailable view and do not load
a manager. Only the selected manager mounts. Navigation entries describe
configured sections, not permission grants: a selected manager can show its
normal no-access state. Statistics uses its own read permission, independently
of content editing.

The optional `content` props are `renderEditor` and `contentHref`; `groups`
accepts `renderContent`; `moderation` accepts `itemHref`, `userHref` and
`sections`; `polls` accepts `freeText` and `maxOptions`. These are the existing
managers' extension points. Without `renderEditor`, body editing uses the
manager's HTML field; the rich text editor is an optional peer dependency.
Changing the client or the provider's viewer/access/language scope resets the
workbench's local navigation and unsaved forms. A URL already selected by the
host stays selected and is checked again under the new scope. When reusing
a transport across sign-ins, keep `ContentKitProvider`'s `viewer` and
`accessRevision` current, or remount the workbench. Media and
version editing remain in the content editor. Site-specific import,
review and publishing workflows can use the individual managers and media
components without adopting this workbench.

## Admin flows

Drop-in staff screens for a host's own admin area: each makes its own reads
and writes, shows loading, empty, error (with retry) and no-access states,
confirms destructive acts and shows only what the caller's permissions allow.
The host writes no API calls. Every flow takes:

| Prop | |
| --- | --- |
| `client`, or `baseUrl` + `auth` | the ContentKit client, or where ContentKit is mounted and auth-ui's `AuthClient` (anything with `authFetch`); else the provider's client |
| `basePath`, `navigation` | multi-view flows: views are paths under `basePath`, filters and pages its query; `navigation` is the host router (`{ pathname, search, navigate(to, { replace }) }`), else the history API; without `basePath`, views stay in memory |
| `guard` | runs every write (auth-ui's `useStepUpGuard()`) |
| `appearance`, `className` | theme `"inherit"` by default: the host's shadcn variables (`--background`, `--primary`, …) and `.dark` |
| `language`, `messages` | the UI language (its bundle loads) and overrides; default the provider's |
| `onError` | every failure the flow shows, for a toast or a log |

Each has an access hook (`use…Access`) for hiding its tab: per area, whether
the caller's permission admits it. `useConfirm()` is the flows' promise-based
confirmation (`typed` for acts that cannot be undone).

```tsx
const location = useLocation();
const navigate = useNavigate();
const nav = useMemo(() => ({ pathname: location.pathname, search: location.search, navigate }), [location, navigate]);
```

- **`CommentModeration`**: every comment (deleted, held and rejected ones with
  their text) by content kind or author, to delete, restore or ban its author;
  the review queue; the site's bans; banned words (mask or refuse) and IP
  bans. Sections at `basePath`, `/queue`, `/bans`, `/words`, `/ips`; `?kind`,
  `?author`, `?page`. Props: `contentKinds`, `itemHref`, `userHref`,
  `sections`, `pageSize`. `useCommentModerationAccess()`. The rule sections
  alone: `BannedWords`, `IPBans` (`useBannedWords`, `useIPBans`).

  ```tsx
  <CommentModeration client={contentkit} basePath="/admin/comments" navigation={nav} contentKinds={["video", "blog"]} />
  ```

- **`PollManager`**: every poll by language, month or day, state (live,
  scheduled, closed, inactive) and question; create and edit through
  `PollEditor` at `basePath/new` and `basePath/{id}`; delete with a typed
  confirmation. Props: `languages`, `freeText`, `maxOptions`, `pageSize`.
  `usePollManagerAccess()`.

  ```tsx
  <PollManager client={contentkit} basePath="/admin/polls" navigation={nav} languages={["en", "ja"]} />
  ```

- **`ContentManager`**: the items of one `kind` the caller manages, by
  state and text, deleted ones to restore; title, slug, description,
  schedule, cover and body with inline images per language; publish or
  unpublish; the moderation state. Props: `kind`, `languages`,
  `renderEditor` (`richTextEditor` from `/editor`, or the host's; a plain
  HTML field without it), `contentHref`, `pageSize`.
  `useContentManagerAccess()`.

  ```tsx
  import { richTextEditor } from "@openrails/contentkit-ui/editor";
  <ContentManager kind="blog" client={contentkit} basePath="/admin/blog" navigation={nav} renderEditor={richTextEditor} />
  ```

  `RichTextEditor` (`value`, `onChange`, `uploadImage`) is Tiptap: headings,
  emphasis, lists, quotes, links and images picked, pasted or dropped. Its
  HTML keeps to that schema, so bodies stay a small, predictable subset.

- **`ReaderManifestEditor`**: a reader version's manifest over its published
  pages and audio: scroll anchors, audio cues (matched to pages or anchors by
  file name in one step), background tracks and auto-advance. Page order is
  the version's media order. `ContentManager` shows it under each version of
  a `reader` kind. Props: `content`, `version`.

- **`MediaItemPanel`**: one video item for its editors: encode state and
  progress, failures with their reason, replace the source, subtitle
  sidecars (language, label, forced), the poster from a frame or an image,
  and regenerate (retries failed uploads). Props: `item`, `source`,
  `subtitles` (path pattern, `null` hides), `poster` (`null` hides),
  `posterPreset`, `languages`. Access is the item's: the host's `CanUpload`.

  ```tsx
  <MediaItemPanel client={contentkit} item={{ kind: "video", id }} languages={["en", "ja"]} />
  ```

- **`UploadQueue`**: many files, each into a new item from `createItem(file)`
  (the host's ref): progress, pause, retry, a leave-page guard, and resume:
  an unfinished upload is kept in `localStorage` (`storageKey`), and adding
  the same file after a reload continues it in the same item. Props:
  `createItem`, `path`, `accept`, `concurrency`, `onUploaded`, `storageKey`.

  ```tsx
  <UploadQueue client={contentkit} accept="video/*" createItem={(f) => api.createVideo(f.name)} onUploaded={(ref) => open(ref)} />
  ```

  The per-item queue engine behind `useUploadQueue` is `ItemUploadQueue` in
  `/client`.

- **`GroupManager`**: groups per kind (tabs at `basePath/{kind}`): by
  letter, order and language; a new group at `basePath/{kind}/new`; a group
  at `basePath/{kind}/{id}` with `GroupEditor` and `GroupMembers`. Props:
  `groupKinds` (default the model's site-owned ones), `kinds`, `labels`,
  `contentKind`, `renderContent`, `pageSize`. `useGroupManagerAccess()`.

  ```tsx
  <GroupManager client={contentkit} basePath="/admin/groups" navigation={nav} labels={{ artist: "Artists" }} />
  ```

- **`UserCommentStanding`**: for a user page, the person's site-wide comment
  ban (ban, replace, lift) and their recent comments (delete, restore). Props:
  `user` (their id), `name`, `itemHref`, `pageSize`.
  `useUserCommentStandingAccess()`.

  ```tsx
  <UserCommentStanding client={contentkit} user={u.id} name={u.username} />
  ```

## Errors

Every call rejects with `ContentKitError`: `code` (the server's, or `network`,
`server_unavailable`, `storage`, `aborted`, `resume_mismatch`, `decode`,
`render_timeout`), `status`, `retryAfter`, `details` (image and video limits),
`blobs` (`not_uploaded`), `ban` (`comment_banned`), `action` (`rate_limited`)
and `unknownCode` (a code this client doesn't know; the message is the
server's), plus `refusal`, `isLimit`, `isCeiling`, `blocksQueue` and
`transient`. Every code has a message in every bundle (`useMessages().error(e)`).

Failures are classified like auth-ui's and billing-ui's (`transportFailure`):
no server reached is `network`; a 502, 503, 504 or 520–524 without an API
error body, or a timeout, is `server_unavailable`.

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
pnpm check                # typecheck, lint, unit tests (pure math and client-free components), build
pnpm build
pnpm test:e2e             # the integration project and the browser specs against a real ContentKit
pnpm test:integration     # or either alone
pnpm test:browser         # (demo/ in Chromium, light/dark × desktop/mobile screenshots)
pnpm demo                 # the stack with seeded demo pages, until Ctrl-C
```

The suites need Docker, Go and ffmpeg. `e2e/server` composes the real
ContentKit's engine (content, groups, codes, media upload and read) with
AuthKit for identity over PGroonga PostgreSQL 18, MinIO, and media-worker and
media-gateway built from this checkout (`e2e/compose.yaml`). Each run brings
the stack up under its own project name and takes it down; the server exits
with its runner. Pages sign in with auth-ui and pass `auth.authFetch`. Tests
set up through `/__test`: accounts (`staff`, `moderator`, `editor` roles),
items the host's resolver knows, stored-object checks, and faults (no CORS,
429 and expired tokens at the gateway, dropped or held bucket requests, API
error replies).

The wire types, route table and error codes in `src/client/generated/` are
generated from ContentKit's route catalog (`pnpm contract`; CI runs
`pnpm contract:check`). UI primitives come from
`pnpm dlx shadcn@4.21.0 add …` (`components.json`); local edits are marked
`// Local:`. The `release` workflow publishes the package to npm for every
published `v*` release. For a UI-only patch, publish an `sdk/ui/vX.Y.Z` release;
this uses the same checks and trusted npm publication without tagging the Go modules.


## Notifications

`NotificationInbox` and `NotificationBell` use the authenticated ContentKit inbox.
The host supplies `renderItem(item)` for its own kinds and optional
`renderActions(item)` for domain links/actions. The components supply cursor
pagination, All/Unread/Archived filters, read/unread, archive/restore, errors and
retries, and actual tracked-work status. Reading or archiving never resolves
work or acknowledges a producer event stream.

```tsx
import { NotificationBell, NotificationInbox } from "@openrails/contentkit-ui";
import { ContentKitProvider } from "@openrails/contentkit-ui/react";

<ContentKitProvider client={client} viewer={user.id} accessRevision={user.accessRevision}>
  <NotificationBell renderItem={renderNotification} />
  <NotificationInbox renderItem={renderNotification} renderActions={renderActions} />
</ContentKitProvider>
```

Handle unknown kinds in the renderer. No billing-specific content is copied into
these components. The bell uses an unread-present dot and polls while visible
(default 30s, `unreadRefreshInterval={false}` disables it). Inbox pages do not
poll by default. Call `client.notifications.refresh()` when your application's
event channel announces new data. `useNotifications` and `useNotificationUnread`
are available from `/react` for a custom presentation. Keep provider viewer and
access revision current; signed-out viewers do not fetch an inbox.

### Reader header controls

`SearchBox` is an inline input with a downward suggestions/recent-search panel,
including loading and empty states. Set `presentation="header"` for a portal
panel directly below the input, matching its width and following it on resize
or scroll: a dimmed backdrop, text rows with colored type icons, and no
thumbnails. Recent-search dates appear when the panel is wide enough. The panel
slides downward when opened and honors reduced motion. Its mobile/desktop
scroll limit is 70vh/60vh. The default inline
presentation is unchanged. Both presentations keep the provider's result order;
the host owns search-page navigation.
`ReaderSettings` combines language selection and content filters in one dialog:

```tsx
<SearchBox presentation="header" onSearch={(query) => navigate(`/search?q=${encodeURIComponent(query)}`)} />
<ReaderSettings
  languages={[{ value: "en", label: "English" }, { value: "es", label: "Español" }]}
  language={language}
  onLanguageChange={changeLanguage}
/>
```

Render these under `ContentKitProvider`. Keep its `viewer` current: guests can
change language, while signed-in readers also see the site's configured content
filters. Filter toggles save through ContentKit immediately. The language callback
belongs to the host so it can update its route and stored preference together.

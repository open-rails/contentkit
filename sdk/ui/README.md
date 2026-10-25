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
  <ContentKitUiProvider appearance={{ theme: "inherit" }} language={lang} density={[1, 3]}>
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
  queue and entity listings a `cursor`); `contents.manage` lists what the caller
  manages (`kind`, `state` incl. `deleted`, `q`); `catalog.totals({kinds, languages})`
  counts what is open; staff lists are `polls.adminList` (`state`, `q`),
  `comments.adminList` (`contentKind`, `author`); `entities.members(id)` pages
  an entity's memberships, `entities.ofContent(id)` / `setOfContent` a content's,
  `setSharedOfContent` only the parent links and `setOfVersion` only one version's
  links. These replacements are atomic and preserve other versions; their `create`
  entries also attach to that same scope.
  `entities.uploadImage(entity, "cover", file)` its cover; `client.kinds()` the content model.
  `contents.manage({kind: "gallery"})` uses `/manage/gallery`; omitting `kind`
  lists manageable content across kinds at `/manage`.
- **Content deletion is version deletion.** Call `contents.removeVersion(id, versionId)`;
  deleting the last undeleted version also soft-deletes its parent atomically.
  This includes hidden single-version kinds: use the ID in `content.versions`.
  Restore that edition with `contents.updateVersion(id, versionId, {state: "published"})`;
  its automatically deleted parent returns to its earlier draft or publication schedule.
  `contents.restore(id)` is for an independently hidden parent, such as moderation.
  Permanent purge follows the configured deletion retention (30 days by default).
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
  the poll media kind. Declared-public covers and previews stay public regardless
  of release visibility; private body images require signed access. `useContent`
  shows an unpublished item's private images to its editors (below).
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
  translate hook), `density` (default `[1, 3]`), `inlinePreview` (default true).

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
again shortly before the URLs expire, waits out `rate_limited`, observes SSE
while uploads process (one authenticated stream per item and account), and reloads after any
commit through the same client. `window` splits a long item into reads of
`chunk` files (default 50) merged into one result. Only the requested windows
stay active and refresh. Leaving a window releases its subscription, timer and
unused in-flight request; revisiting it obtains a fresh read. One prior result
supplies placeholder metadata while the next range loads, then is replaced.
That placeholder is discarded after an error or item, client or account-scope
change. There is no passive window cache. An explicitly requested large range
(such as an entire editor folder) still reads every window in that range.
`refresh()` joins a read in flight (a player's grant refresh);
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

Every card list (`ContentGrid`, `ContentListing`, `ContentRail`, `DailyReleases`,
`SearchResults`, `ContentHistory`) and `ContentCard` take `decorate(card)`:
the host's badges, drawn over the cover (place them yourself; clicks reach
the card, and click signals stay intact). Cards carry what badges need:
`access.level` and `access.tiers` (the effective rule, the same for every
reader), `version.has_audio`, `viewer.favorited` and `entities`.

Hosts can tailor the details without replacing the card. For example,
`metadata={{ languages: false, traits: ["colored"], duration: false }}` keeps
the page count and the Colored trait, and `showCounts={false}` hides reaction
totals. `entityKinds={["series"]}` selects series chips;
`filterEntity={(entity, kind) => ...}` filters them before the chip limit.
Defaults continue to show all normal metadata, entity chips and totals.

For a poster-style card, set `aspectRatio="182 / 260"`,
`titlePosition="cover"` and `entityStyle="text"`. The title is a single
accessible link over the cover's bottom gradient; entities remain clickable
without chip borders. The ratio overrides `shape` and also applies to loading
placeholders. These options work through every card grid, rail and daily feed;
omitting them retains the default ratio, title below the image and chips.
Plain-text entities sit directly below the cover, followed by other metadata.

```tsx
<ContentGrid query={{ kind: "gallery" }}
  aspectRatio="182 / 260" titlePosition="cover" entityStyle="text"
  entityKinds={["series"]} showCounts={false}
  metadata={{ languages: false, traits: ["colored"], duration: false }} />
```

Card grids use **width-driven tiling**: preserve roughly the same apparent
card size as the container grows, adding or removing columns to use the
available space. The application supplies its card-width target; ContentKit
UI derives the column count. Cards may grow slightly to fill each row.

For example, `grid={{ minCardWidth: 280, mobileColumns: 2 }}` keeps two columns
on small screens and a stable 280 CSS-pixel minimum on desktop, with no column
ceiling. This shared behavior applies to card grids, daily releases, Popular
and `ContentRail` grid previews; an unconfigured rail remains horizontal.

Use a stable CSS-pixel width for ordinary listing cards. `maxColumns` is an
explicit opt-in for surfaces that intentionally cap their columns.
`minCardWidth` also accepts CSS lengths for application-specific layouts.

Custom page-thumbnail or media grids can use the same primitive: call
`cardGridLayout({ minCardWidth: 280, mobileColumns: 2 })` and apply its
`className` and `style` to a `ContentKitUiRoot`. This keeps their track widths,
gaps and responsive behavior identical to content-card lists.

`ContentGrid` and `ContentListing` take `locked`: `show` (default) marks the
cards the reader doesn't open, `hide` leaves them out, and `collapse` replaces
locked cards with a cover-sized summary tile. Opening it reveals the cached
cards in their original ranking order, with a reduced-motion-aware animation.
The locked and optional-filter tiles use the same component as daily releases.

`ContentGrid` and `SearchResults` request `summary=true` on the first page.
`useContents` and `useSearch` expose that selection's `summary` across pagination:
the distinct `filtered_count`, overlapping `filtered_entities` counts, and
`locked_count` before cursor pagination. All counts retain the query's hard
domain, language, entity, access and popularity-window eligibility. A bounded
keyword selection may report `truncated`; its tiles and contextual filter
counts then show `≥` to identify lower bounds. Clicking a filtered tile opens
the shared filter dialog with this selection's per-entity counts.

Preview policy belongs to the host. Set `showFilteredSummary={false}` to omit
that tile, `lockedSummaryMinColumns={6}` to offer a locked tile only when six
grid columns fit, and `cardLimit={6}` / `mobileCardLimit={4}` to limit mounted
preview cards. `summaryLabels` supplies the host's nouns for both tile labels.

### Daily releases

```tsx
<DailyReleases query={{ kind: "gallery", days: 10 }} initialVisible={30} />
```

`DailyReleases` fetches all card metadata for the calendar window in one
`GET /releases` request. It uses the browser's IANA time zone unless
`query.time_zone` is supplied. Days appear newest first as keyboard-accessible
disclosures, with reduced-motion support. The initial card budget is shared
across days, newest first. Opening older days or choosing **Show more** reveals
already-fetched cards; it does not make another listing request.

To browse old releases, set `query.end_date` to an inclusive `YYYY-MM-DD` in
the listing's time zone. The same bounded window ends on that date; it does
not load intervening years. Omit it for the latest releases, or combine it
with `days: 1` for one exact day. Future and invalid dates return HTTP 400.
Current visibility and access rules still apply to historical windows.
`ReleaseDatePicker` opens a themed calendar popover with month/year dropdowns,
so older dates need no month-by-month paging. It selects one day, disables
future dates, and closes after selection; there is no typed input or reset
button. Keep its controlled value in your router for shared URLs and
back/forward navigation. The app chooses placement and the listing window:

```tsx
<ReleaseDatePicker value={date} onValueChange={setDate} />
<DailyReleases query={{ kind: "gallery", days: date ? 1 : 10, end_date: date || undefined }} />
```

`isReleaseDate(date)` validates URL input before rendering the feed; the picker
shows an error for invalid or future dates. `localReleaseDate()` provides
today's browser-local date; supply the picker's `max` when the listing uses a
different time zone.

Only revealed cards occupy the grid, and images/previews mount when those cards
enter the viewport. Collapsing a day removes its media. The standard card
properties (`href`, `decorate`, `entityKinds`, `tiers`, and so on) remain available.
For a custom layout, `useDailyReleases(query)` exposes the same shared read and
`client.discovery.releases(query)` exposes the typed request directly.

Opt into `collapsePremium` to replace a day's locked tier previews with one
counted tile at their first original position. Opening it restores their order
within the existing card budget; **Show more** still controls the rest. This
only reveals previews and never grants access. Native layout/reveal animations
respect reduced motion, and keyboard activation focuses the first revealed
card. `summaryLabels.premium(count)` and `.filtered(count)` can supply a host's
nouns; the shared defaults use localized item labels.

Each day's `filtered_count` is a distinct-content total hidden only by the
reader's toggleable preferences. A filtered tile opens `ContentFilterDialog`
with `filtered_entities` counts for that day and the same configured controls
as `ReaderSettings`. Per-entity counts may overlap; they must not be added to
compute the total. Changing a preference refreshes the feed and counts while
the dialog remains open. Hard domain rules and internal classifications are
never controls or count labels. A day with only filtered items remains visible.
For custom interfaces, pass selection-specific `counts` to
`ContentFilterSettings` or the controlled `ContentFilterDialog`; omit `counts`
for ordinary settings without listing totals.

`published_within` filters a listing by publication age independently of its
ranking window. For example, `{ sort: "popular:7d", published_within: "7d" }`
ranks only content published during the past seven days.

`<PopularContentGrid query={{ kind: "gallery" }} />` supplies four visible tabs
for 7-day, 90-day, 365-day and all-time activity and loads further pages as the
reader scrolls. Publication age is unrestricted, so older content can trend
again. Supply `query.published_within` only when a release-age cutoff is wanted.
`period` and `onPeriodChange`
optionally keep its state in the host's URL. For a homepage preview, pass
`popularQuery("7d", { kind: "gallery", limit: 6 })` to a `ContentRail`'s `list`.

For a historical activity window, pass `period={{ from: "2026-04-01", until:
"2026-05-01" }}` and `query={{ kind: "gallery" }}`. Dates are UTC, with an
inclusive start and exclusive end. No preset tab is selected for a custom
range; selecting a tab calls `onPeriodChange` so the host can replace the range
in its URL with that rolling preset. `popularQuery` accepts the same range
object and sends `sort=popular&activity_from=...&activity_until=...`.

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

## Entities

Tags, artists, series, characters, channels and rails: the kinds the site's
content model declares. `useKinds()` reads it (`GET /v1/kinds`); until that
route is served, pass the host's model as `initial` (and `kinds` to the
components below).

```tsx
const { items, hasMore, loadMore } = useEntities({ kind: "artist", letter: "a", sort: "count" }); // parent, scope, contentKind, language
const g = useEntity(id);           // g.entity: names, text, attributes, parent, scope, children, edges, counts
await g.update({ attributes: { artist_type: "circle" } }); // staff: create, names, edges, merge, remove, restore
await g.uploadCover(file);         // into the entity kind's folder (cover/{name}), then selected
const members = useEntityMembers(id);       // add, remove, reorder
const content = useContentEntities(contentId); // content.set(memberships, kinds)
```

- **`EntityDirectory`**: one kind's entities with A–Z buckets (`letter` and
  `onLetterChange` to keep it in the URL), content counts, `layout` list or
  grid, paged. **`EntityHeader`**: an entity page's cover, names, description,
  typed attributes, parent, scope and sub-entities. Links go to each entity's
  canonical path (`href` overrides) through the provider's `navigate`.
- **`EntityEditor`** (staff): create, or edit slug, parent, scope, declared
  attributes, names and aliases per language, description and sort key per
  language, cover, related entities; `EntityMergeDialog`; delete and restore.
- **`MembershipEditor`** (staff): a content's entities per entity kind, with the
  version (`versions`), role, position and window each kind allows; one save.
  **`EntityMembers`**: an entity's members, reordered, windowed, used as cover.
- `labels` names kinds, attributes, roles and values by identifier; others
  show humanized. Covers render `cover.renditions`; `renderCover` stands in
  until entity reads carry them.

## Reference administration workbench

`ContentAdmin` is an optional library component composing the existing content,
entity, moderation, poll and statistics managers. It is not a standalone server
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
  sections={["contents", "entities", "moderation", "polls", "statistics"]}
  contentKinds={["article", "video"]}
  labels={{ article: "Articles", video: "Videos" }}
  content={{ renderEditor: richTextEditor }}
/>;
```

The client is an existing ContentKit client, or pass `baseUrl` and the host's
`auth` transport. `guard`, `language`, `messages` and `onError` follow the
individual managers' contracts below. Without `navigation`, the workbench uses
browser history; without `basePath` as well, navigation stays in memory.

`GET /kinds` supplies languages and configured content and entity kinds. Omit
`contentKinds` to offer all content kinds; omit `entityKinds` to offer the
site-owned entity kinds. Explicit lists restrict and order those choices, and
unknown kinds are omitted. The routes beneath `basePath` are
`contents/<kind>[/new|/<id>]`, `entities/<kind>[/new|/<id>]`, `moderation`,
`polls[/new|/<id>]` and `statistics`; each manager retains its own filters and
subviews. Unknown or excluded routes show an unavailable view and do not load
a manager. Only the selected manager mounts. Navigation entries describe
configured sections, not permission grants: a selected manager can show its
normal no-access state. Statistics uses its own read permission, independently
of content editing.

The optional `content` props are `renderEditor` and `contentHref`; `entities`
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

## Original and edition titles

A host can set `title_policy: original_and_version` on a content kind. The
parent then carries optional `original_title` and `original_language`, while
`version.text[language].title` is the edition's display title. The reader editor
shows one shared Original title and a Display title bound to the selected
version's language; changing one edition never rewrites sibling titles.

The default `localized` policy retains generic `content.text[language]`
behavior. Those stored texts are preserved under the edition policy too.
Public titles resolve from the selected edition, then the original, then the
legacy localized title. An original's language may remain unknown; do not label
an existing English translation as the source title.

For existing content, `catalog.BackfillVersionTitles` is an explicit migration
helper: it copies only missing titles from exactly matching content languages
and does not overwrite existing edition text or invent source titles. Hosts
call it from their migration command, rather than server startup.

## Daily readership statistics

`StatsDashboard` charts distinct engaged readers for the past ten UTC days.
It reads `GET /stats/activity?kind=gallery&days=10`; opening a page without
reading does not count. A subject needs at least one second of measured active
reading and nonempty page coverage. Counts are distinct per day, not across the
whole period, and the same anonymous person using different browser identities
can count more than once.

The host defines audience groups on the server, for example:

```go
cfg.ActivityCohorts = []signal.ActivityCohort{
    {ID: "supporter", Identity: signal.ActivityAccount, AnyTier: []string{"paid"}},
    {ID: "member", Identity: signal.ActivityAccount},
    {ID: "guest", Identity: signal.ActivityAnonymous},
}
```

The first matching group wins for each subject/day. Tier eligibility comes
from trusted observations on that day; an account that gains a matching tier
is counted once in that group, even if it also read before upgrading. A host
that omits groups gets only the overall total. Unmatched readers also remain
in that total. Labels, colors and display order belong to the host UI:

```tsx
<StatsDashboard kinds={["gallery"]} days={10} series={[
  { id: "guest", label: "Guests", color: "#3987e5" },
  { id: "member", label: "Members", color: "#e98042" },
  { id: "supporter", label: "Supporters", color: "#21a87e" },
]} />
```

The endpoint returns `cohorts: string[]` and each day's `cohorts` count map,
`readers` total and `active_ms`. The chart uses line series, zero-filled days,
an accessible daily table and no built-in commercial audience categories.

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

For reader content, each version has a canonical editor URL under the manager:
`<basePath>/<contentCode>/versions/<versionCode>`. An older content-only URL
opens its first version. Shared gallery text and entities are explicitly marked;
version traits, credits, files, chapters and reading metadata edit only the
selected version. Version navigation warns before discarding unsaved changes.
The host still owns navigation outside the flow, including browser Back handling.

- **`ContentManager`**: the items of one `kind` the caller manages, by
  state and text, deleted ones to restore; title, slug, summary/description,
  schedule, cover and body with inline images per language; publish or
  unpublish; the moderation state. Props: `kind`, `languages`,
  `renderEditor` (`richTextEditor` from `/editor`, or the host's; a plain
  HTML field without it), `contentHref`, `pageSize`, `noun` (the kind's
  name, localized by the host as it reads mid-sentence, e.g.
  `{ one: "gallery", other: "galleries" }`: the list heading, with its first
  letter capitalized, the New button and the empty state; generic wording
  without it). `useContentManagerAccess()`.

  Access badges and editors use the selected edition's rule, including a kind's
  hidden single edition. Existing access changes target that edition; list rows
  offer no parent-wide access toggles or bulk access mutation. A new item's
  chosen access rule becomes its first edition's creation default.

  A reader kind's editor keeps its save, undo and publish actions pinned as
  the page scrolls. A host with its own sticky header sets
  `--ckui-sticky-top` on an ancestor to that header's visible height, so the
  actions stop below it; `appearance.variables.stickyTop` sets a fixed value
  instead and takes precedence.

  Post editors label the preview text **Summary (optional)**. A supplied
  summary becomes the list excerpt; otherwise ContentKit extracts the opening
  plain text from the post body (HTML, plain text or Quill Delta), without image
  embeds or formatting metadata. The shared storage/API field remains
  `description`, also used by other kinds. The list API exposes the resolved
  `excerpt`; `PostList.excerptLength` limits its displayed length (220 characters
  by default). A locked post never exposes its body through this fallback.

  ```tsx
  import { richTextEditor } from "@openrails/contentkit-ui/editor";
  <ContentManager kind="blog" client={contentkit} basePath="/admin/blog" navigation={nav} renderEditor={richTextEditor} />
  ```

  A `reader` kind uses one gallery workspace: localized metadata, configured
  entities, access, cover, ZIP/pages, ordering and publication are available
  together. A new draft is allocated internally on the first save or upload;
  its URL change keeps the current fields and upload queue mounted. Each version
  has its own page, with a separate chapter section and optional audio/reading
  tools. A chapter selects its starting page; its end follows the next chapter.
  Page references use original source hashes, while page numbers follow manifest
  order. **Publish** saves the visible edits and any dirty tools first and
  checks every media window; failed uploads, processing or refused saves keep
  the work unpublished. **Save draft** can save metadata while files upload.

  `RichTextEditor` (`value`, `onChange`, `uploadImage`) is Tiptap: headings,
  emphasis, lists, quotes, links and images picked, pasted or dropped. Its
  HTML keeps to that schema, so bodies stay a small, predictable subset.

- **`AdminQuickSearch`**: autocomplete navigation to content and entity editors,
  including managed drafts and hidden content. Supply the permitted
  `contentKinds` / `entityKinds` and mandatory `contentHref` / `entityHref`
  callbacks. Suggestions without an editor URL are omitted; public paths are
  never a fallback. It reuses `SearchBox` geometry and arrow/Enter/Escape
  controls, with no recent-search history or free-text results submission.
  Management authorization remains enforced by the server. Set
  `presentation="header"` for the shared centered-header dropdown.

- **`PageNotices`**: quiet, collapsed notes about automatic page changes:
  pages removed as blank, duplicate or covered by a spread (one-click Restore)
  and archives put in name order (Dismiss). It renders nothing without notices
  and never blocks saving or publishing. `ContentManager` includes it for reader
  versions; `usePageNotices()` exposes the same API. Page tiles retain their
  filenames and show a separate position counter. Dragging changes order;
  renaming and removal remain local until **Save** in the
  reader workspace. Undo/Redo spans those edits and its metadata drafts.
  Uploads and cover processing can store files earlier; Undo is not a rollback
  of bytes already uploaded. The header contains the cover selector and the
  selected edition's staged release time. `access_scope: version` presents a
  direct edition access rule instead of a parent rule/inheritance toggle.
  The shared Save includes edition details, chapters and reader metadata.
  Embedded editors hide their own Save/Revert actions and do not save when
  Enter submits a field. Undo/Redo remains shared across their drafts.
  Unpublish and Delete affect the selected edition. Product and price changes
  belong to OpenRails. Standalone `VersionEditor` exposes explicit Publish/Unpublish
  actions that save staged edition fields with the state change in one write;
  the parent item's publication remains separate. Deleted editions are read-only
  and restore to Draft through an explicit Restore version action. The Trash
  links to edition management for visible-version kinds; parent deletion and
  restoration remain for hidden-version kinds. Trait toggles are omitted in the reader workspace; version
  tags use the ordinary entity picker. Reading layout lives under Reader tools
  when the kind declares `manhwa_style`; Paged / Continuous vertical stages that
  existing reader behavior until the same Save. Kinds with an audio upload show
  an Audio section beside Chapters for cues, anchors and background tracks. The display-title field follows
  the edition's staged language, and preserves titles in other languages.

- **`ReaderManifestEditor`**: a reader version's manifest over its current
  pages and audio uploads, published or not, seeded from the stored manifest:
  scroll anchors, audio cues (matched to pages or anchors by file name in one
  step), background tracks and auto-advance. Page order is the version's media
  order. `ContentManager` shows it under each version of a `reader` kind.
  Props: `kind`, `content`, `version`.

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

- **`EntityManager`**: entities per kind (tabs at `basePath/{kind}`): by
  letter, order and language; a new entity at `basePath/{kind}/new`; an entity
  at `basePath/{kind}/{id}` with `EntityEditor` and `EntityMembers`. Props:
  `entityKinds` (default the model's site-owned ones), `kinds`, `labels`,
  `contentKind`, `renderContent`, `pageSize`. `useEntityManagerAccess()`.
  Its staff listing includes internal entities, with an “Internal use only”
  setting in the editor. Public directories and search never include those
  entities. Use `management` on a standalone `EntityEditor` or `EntityPicker`
  to opt into the same staff API; ordinary user-owned editors stay public.

  ```tsx
  <EntityManager client={contentkit} basePath="/admin/entities" navigation={nav} labels={{ artist: "Artists" }} />
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
pnpm check                # typecheck, lint and built package
pnpm exec vite build --config ../../tests/critical/browser/vite.config.mjs
pnpm test:browser         # actual built SDK and React against the critical real-service fixture
pnpm test                # full reviewed critical contract; fails while replacement remains incomplete
pnpm demo                # separately retained seeded demo stack
```

The browser cases use the services and credentials configured for
[the critical suite](../../tests/critical/README.md). Missing dependencies and
selected skips fail. They run the built package against real scoped ContentKit
and AuthKit; held responses are network fault fixtures. The old unit/integration
entrypoints have been removed. Demo and benchmark support remain separate from
qualification.

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

`SearchDialog` is a controlled search modal with a visible title, a separate
close control, and suggestions that contribute to its height rather than being
clipped by its container. The field and close control stay visible while long
result lists scroll. It closes when a query or navigable suggestion is chosen,
and returns focus to the opener. Hosts supply `open`, `onOpenChange`, and
`onSearch`; title, description, placeholder, appearance and `returnFocus` can
be customized. Use `SearchBox presentation="dialog"` when a host already owns
the dialog shell and only needs the in-flow search layout.

```tsx
<SearchDialog open={searchOpen} onOpenChange={setSearchOpen}
  onSearch={(query) => navigate(`/search?q=${encodeURIComponent(query)}`)} />
```

`SearchBox` is an inline input with a downward suggestions/recent-search panel,
including loading and empty states. Set `presentation="header"` for a portal
panel directly below the input, matching its width and following it on resize
or scroll: a dimmed backdrop, text rows with colored type icons, and no
thumbnails. Recent searches omit dates. The panel
slides downward when opened and honors reduced motion. Its mobile/desktop
scroll limit is 70vh/60vh. The default inline
presentation is unchanged. Both presentations show matching recent queries
first, with prefixes before interior matches and newest first within each tier,
then matching entities, then content titles. A
recent query remains selectable while network suggestions debounce or load.
Guest history persists in localStorage across tabs; account history comes from
the server, fetched on opening rather than on every keystroke. `recent={false}`
omits history. Only completed searches are recorded.

`GET /v1/typeahead` matches literal substrings of a title, entity name or direct
alias, preferring exact names and prefixes. Case, Latin accents and whitespace
are normalized, and complete English filler words `the`, `a` and `an` are
ignored in both autocomplete/history matching and full search. Filler-only
queries return no matches. Identity and slug normalization stay unchanged.
Autocomplete does not expand associated entities, contextual keywords or typos:
Inazuma can suggest that artist, but not Business Question merely because
Inazuma created it. Full `GET /v1/search` retains broader keyword and typo
matching. Visibility, tenant, language and version eligibility still apply.
The host owns search-page navigation and suggestion links.

Search documents store their normalized terms (core migration
`0043_stored_search_terms`); apply normal migrations before serving the new
core. Native query-plan tests verify the indexed Latin/CJK paths against a
50,000-document catalog.
`ReaderSettings` combines language selection and content filters in one dialog:

```tsx
<SearchBox presentation="header" onSearch={(query) => navigate(`/search?q=${encodeURIComponent(query)}`)} />
<ReaderSettings
  kinds={["gallery"]}
  languages={[{ value: "en", label: "English" }, { value: "es", label: "Español" }]}
  language={language}
  onLanguageChange={changeLanguage}
/>
```

Render these under `ContentKitProvider` with `viewer={null}` for guests. Guest
language choices and filter overrides are stored per app in localStorage and
shared across tabs. Account filter choices come from ContentKit's database;
account languages use the provider's `languagePreference` adapter. Each domain
only offers its configured, user-toggleable entities; internal classifications
and required or locked rules never appear in reader settings. Labels use the
entities' localized names. Saving this domain's settings preserves choices for
entities offered by other domains. The configured `applies_to` content kinds
share one preference per entity, so a tag toggle can affect galleries and blog
posts together. `kinds` selects a listing context and displays controls when it
intersects `applies_to`; it never creates a second per-kind preference. Rows use
entity names and have no duplicate type headings.

The HTTP shape is flat and updates only the submitted entities:

```json
{"filters":[{"entity_id":"tag-id","visible":false}]}
```

`PATCH /me/content-filters` preserves omitted entities and `include_deleted`.
`setVisible(entityId, visible)` performs that one-entity update. Reset a choice
with `{entity_id, default: true}`; the reset follows current domain defaults and
suppresses old legacy choices. Independent account updates merge atomically.

Guest storage migrates v1 choices and pending registration snapshots into v2,
keeping the original v1 bytes. Conflicting old per-kind choices resolve to
hidden; explicit scoped resets suppress the older unscoped fallback. New shared
choices override all old values. Registration initialization uses the flat
`filters` array and never overwrites an existing account's settings.

```tsx
<ContentKitProvider
  client={contentkit}
  viewer={user?.id ?? null}
  language={routeLanguage}
  languagePreference={{
    load: (accountId) => loadAccountLanguage(accountId),
    save: (language, accountId) => saveAccountLanguage(accountId, language),
  }}
>
  {children}
</ContentKitProvider>
```

The adapter must check the expected account before reading or writing. Return
null from `load` when no language is saved. A localized URL does not change a
stored preference: only an explicit language selection does. The language
callback still belongs to the host's router.

On a confirmed **new registration**, call
`contentkit.preferences.inheritAfterRegistration(userId)`. Never call it for an
ordinary sign-in. The captured guest choices are retried once that account's
authenticated transport is ready; failed transfers remain pending in localStorage.
Existing account filter records are preserved. A confirmed new registration
inherits the guest language over its initial default; retries preserve a later
language choice. Ordinary sign-ins never import guest preferences.
`useReaderPreferences()` exposes the preferred language, pending/error state and
a retry action. ReaderSettings shows an incomplete transfer and lets the reader
retry it. Guest preferences are retained separately after registration and logout.

### Measured engagement

`useReadingEngagement(contentId, versionId, options)` measures loaded, visible
content while the page is focused. Keep the hook above the horizontal/vertical
reader switch so a mode change retains its visit. Attach `engagement.page(index,
filename)` to the actual image element (zero-based index), or `engagement.video`
to the playing video. `engagement.setResume(filenameOrAnchor)` records the opaque
reader selector separately from measured coverage. Optional
`[data-reader-viewport]` on the reader container clips page measurements to that
viewport, including long-page breakpoint readers.

Opening the route, preloading an image and changing the page selector produce
no engagement on their own. The tracker measures the visible part of each image
and focused active milliseconds; multiple visible pages share elapsed attention.
Static-page attention pauses after 60 seconds without genuine input (configurable
with `idleAfter`). Visible, focused playback continues without requiring mouse
motion, but pauses, buffering and seeks do not become watched coverage. Suspended
timer gaps over five seconds are discarded rather than guessed.

Page changes stay local. Cumulative snapshots flush at most every 120 seconds
(`checkpointInterval` may shorten this), and on hiding/exiting the page or
unmounting the reader. `engagement.flush()` offers an explicit checkpoint.
Each segment captures its own immutable server activity receipt when observation
starts, retaining event-time premium/free access across retries; a later receipt
never reclassifies earlier attention. Revisions replace the same segment, rather
than incrementing visit or page-open counters. Positive short partials are kept
so later measured attention can accumulate; qualification happens server-side.

Use `enabled` to honor the host's consent and authentication-readiness policy.
With an explicitly signed-out provider (`viewer={null}`), positive partials also
save browser-local resume/history across tabs. This remains useful when the signal
plane is unavailable, without inventing remote analytics. Account history stays
server-owned. An account switch discards old in-flight observations instead of
assigning them to the next account. The optional `kind` groups local guest history
only; remote requests never trust a browser-supplied content kind.

`useHistory` retains previously read items for guests and accounts.
Guest cards hydrate through `client.discovery.knownCards({ ids })`
(`GET /content-cards?ids=…`, at most 100 IDs, caller order), rather than a browse
listing. This read records no engagement. Current domain rules, reader filters,
languages, publication/deletion state and access decisions still apply; hidden
items are omitted and locked cards use normal redaction. Published items use
the same domain, filter and access policy in `/contents` and search.


For a custom integration, `EngagementTracker` accepts injected `begin` and
`submit` callbacks. `client.discovery.activityContexts(targets, session, language)`
exposes `POST /activity-contexts`; `client.discovery.engagement(snapshots, options)`
exposes `POST /engagement`. Capture receipts when their segments begin, not when
delayed batches flush. A missing, disabled or invalid receipt prevents remote
engagement submission and never falls back to legacy route-open tracking.

`useExposure(items, options)` separately records cumulative rendered and visible
list placements and attributes clicks to their original ranks. Hidden paid tiles
are not impressions. An explicit item `position` preserves gaps from omitted
cards. Each new placement receives a server context; the client supplies no
premium-status or eligibility claims. End custom `Beacons.exposure` receipt state
with `endExposure(renderId)` when its list leaves the page; `useExposure` does this
automatically. These bounded exposure/click packets serialize keepalive requests;
exit delivery remains best effort.

The staff dashboard and SDK statistics queries use measured people and active
time, with comments ranking retained. See [statistics semantics](docs/statistics.md)
for daily distinctness, qualification and active-time units.

## Billing boundary

ContentKit stores content and the entitlement keys required to access it.
OpenRails owns products, prices, purchases and entitlement grants. The access
and version editors change content rules; they do not edit a second billing
catalog. Use OpenRails or billing-ui directly for product and price management.

`Paywall` and `MembershipBox` call the host's `onCheckout({ key })` with an
entitlement key. The host resolves it through OpenRails and opens its billing
flow. These components never create free grants or infer a price. The purchased
content library remains available as `client.discovery.library()`.

## Upload and publication

Create the content and version first, then use the normal media upload API:
`client.media.upload(file, { ref, path })` presigns and transfers bytes, and
`client.media.commit(ref, ops)` attaches them and queues media processing.
ZIP extraction, thumbnails and other renditions run in the normal media
workers; their existing media progress remains available.

Uploading never publishes the content. Edit its metadata and call
`client.contents.publish(id, publishAt)` separately to accept durable publication.
The response may still have `live: false`: the server finishes processing and
storage publication in the background. Editor `version.publication` records
pending, waiting, running or failed work independently of an existing live
release. Reopening the editor reads that durable state. `useContent` observes
accepted publication until it completes or reports failure; it never unlocks
content by advancing the browser clock.

`publishWhenReady` submits once and observes due publication work for a bounded
time, including amendments whose previous release remains live. It returns the
latest content on failure, timeout or a future schedule; it never retries the
publication POST while processing continues.
The bulk uploader creates private drafts and follows the same upload/commit
flow. It has no publish-on-completion setting or publication status polling.

Chapters use start markers naming virtual manifest files:

```ts
await client.contents.setSegments(galleryId, versionId, [
  { title: "Chapter 1", start_page: "pages/001.jpg" },
  { title: "Chapter 2", start_page: "pages/020.jpg", release_at: "2026-11-01T12:00:00Z" },
]);
```

Manifest order determines each chapter's files up to the next marker or the
end. Inserting `017c.jpg` or `019b.jpg` before `020.jpg` includes those files in
chapter 1 automatically. Hashes and original names remain file metadata; they
are not chapter membership lists. Preserve a returned chapter `id` when editing
its marker or title. Reordering files moves chapters with their markers; titles
and schedules follow the marker. Missing or duplicate markers require correction
instead of silently selecting another file.

Every chapter shares its version's access policy. Publication moves the selected
files from that version's `future` area into `public` or `private`; one item
token opens its private folder and never a sibling version. Reader plans expose
completed chapters and an optional `next_release_at` observation hint, without
revealing future file paths. A due hint triggers another read, not local access.

The bulk upload queue retains the exact commit batch until the server confirms
it. A retry reuses that batch ID and ordered operations. Browser persistence
is scoped to the API mount and the provider's explicit viewer ID; without a
known viewer, retries remain in memory only. Switching accounts aborts and
clears the visible queue, without resuming the other account's work.

`MediaReadinessNotice` uses shared `useMediaRead` SSE observation with an
editor-only status read, without signing image URLs. It shows ZIP extraction,
per-preset completion counts, encoding progress and failures. Observation stops
when the item is ready or storage capacity blocks progress. Reopening the
editor reads current server state; browser upload records are not processing
state. Account or item changes and unmount cancel the stream. A disconnect
reconnects up to three times with its cursor; retention expiry explicitly sends
a fresh `resync` snapshot. Exhausted reconnects show a retry action. There is no
processing poll option; signed URL expiry refresh remains independent.

`client.media.watch(ref, receive, signal, after?)` uses the client's normal
fetch, headers, token and credentials. It delivers `snapshot`, `progress`,
`resync` and `heartbeat` events; the latter carries only a cursor. It makes one
connection, leaving reconnect ownership with its caller. Upload `waitFor` and
`editorView` also use these updates, with their existing timeout and retry limits.

### ZIP upload at an admin entrypoint

`ContentUpload` is the same model-driven upload form used by the content
manager's bulk uploader. A host supplies its content kind, labels, defaults and
navigation; ContentKit creates drafts and chooses upload paths from that kind's
rules. It does not publish on completion.

```tsx
<ContentUpload
  client={contentkit}
  kind={galleryKind}
  labels={labels}
  defaults={{ language: currentLanguage, format: preferredFormat }}
  onOpen={(draft) => navigate(galleryEditorURL(draft))}
/>
```

The transfer stage shows actual callback bytes, such as `5 MB / 30 MB`.
After durable commit, retained processing snapshots supply ZIP extraction and
separately labelled rendition counts; unrelated presets are never summed into
an overall image count. Completed thumbnail derivatives appear while remaining
sources show placeholders. A bounded preview requests URLs for at most twelve
sources.

`MediaProcessingPanel` can also be mounted for an existing content/version ref:

```tsx
<MediaProcessingPanel client={contentkit} item={versionRef} />
```

This panel and the reader/gallery editor recover their counts and thumbnails
from the server after reopening. Browser storage is used only to resume an
unfinished transfer or retry its exact commit, not as processing state. The
host's regular editor retains the explicit Publish action.

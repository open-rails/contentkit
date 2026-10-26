# Bulk import

Legacy importers write through `POST /v1/import` (route group `import`, staff
permission `root:import:write`; Go: `Client.PostImport`), never into
ContentKit's tables. The body is an `importer.Batch`; its wire types are in
[`api/openapi.json`](../api/openapi.json).

## Identity and convergence

- Every record with an identity carries `external_id`, scoped by the batch's
  `source` and its entity (`content`, `version`, `group`, `channel`, `comment`, `poll`,
  `poll_option`) in `content_external_ids`. Re-sending a record updates what
  changed and writes nothing when nothing did; it is never duplicated.
- `id` (a UUIDv7) is kept when the external id is new; derive it with
  `contentref.LegacyID` so media folders survive a re-import into a fresh
  store. An external id already imported under another id is a `conflict`.
- References name a record by `id` or `external_id`
  (`{"external_id": "folder:123"}`); `target` names a `content`, `version`,
  `group`, `channel` or `comment`, as the section allows.
- Collections given replace what is stored (creators, version text,
  names, group text, edges, poll options); omitted ones stay.
  Versions not listed stay. `memberships` replace a content item's
  memberships in the group `kinds` they name.
- Content `code` and group `code` keep an existing content code; a version's
  `code` keeps its version code (`?v=`). Without one, a stored record keeps
  its code and a new one is assigned one; a code another record holds is a
  conflict.
- `aliases` record the old site's identifiers; hosts 301 old URLs through
  `POST /v1/codes/aliases` (`Client.PostCodesAliases`), which answers each
  `(source, kind, key)` with `found` (link, canonical path, locator),
  `not_found` (unknown or hidden) or `gone` (removed), following merges.
- Raw opens and legacy aggregate counters are not importable. Preserve existing
  bookmarks in `legacy_reading`: target a content item or version, identify one
  user or anonymous key, and send the source's last-read time, progress and resume
  selector. Version bookmarks also restore the work's history. These rows restore
  navigation only; they never create measured readers, quality, completions or rank.
- Imported content is trusted: not screened, approved. Reactions and
  favorites take fresh preference revisions; `preference_floor` first raises
  the sequence past a retained signal plane (also the restore step).
- Import authoring channels through `channels`, separately from descriptive
  `entities`. A channel names its `auth_scope`, or the batch enables
  `provision_scopes`. ContentKit calls AuthKit synchronously after committing
  channel identity; a provider failure is reported for that record. Retry the
  same external ID to finish without creating another channel or group.
- Records of erased subjects fail (`erased`).
- A record gone from the source is deleted only by a `deletes` record
  (`entity`: content, version, group, channel, comment or poll; `external_id`), never
  by its absence from a batch. It deletes as the delete route does; one
  already deleted (or since erased) stays as it is, so re-sending converges.
  An external id never imported fails `not_found`.
- A persistent sync finds those deletes with `POST /v1/import/external-ids`
  (`{source, lists: [{entity, cursor, limit}]}`): per entity, the external ids
  the source's imports hold with their ids and `deleted`, in external id
  order, up to 10,000 a page. Send a delete for each one not deleted that the
  source no longer has.

## Order and failures

Sections are written in order: entities, channels, contents, memberships,
aliases, comments, reactions, favorites, polls, votes, signals,
deletes; a
record may name what an earlier section or call wrote. Each section runs in
transactions of up to 1000 records: all at once, else each in its own
savepoint, so the result lists only the records that failed
(`section`, `index`, `external_id`, `code`, `error`) and the rest commit.
Fix the cause and send them, or the whole batch, again. A batch holds at
most 20,000 records.

Signals are skipped without a signal plane; `skipped` counts them. Products
and prices are managed through OpenRails, not the ContentKit import.

## After an import

Imports update content facets and interaction counts in their writing
transactions, and queue search-document updates. Entity counts and automatic
covers derive from current facets on reads; there is no periodic recount.
Channel lifecycle changes finish through direct idempotent AuthKit calls. There
is no channel-sync worker. Incomplete channels remain unavailable for authoring.

## Media

Media is not part of the batch. Fill each imported version's folder through
the upload routes, as the host (`Engine.Client()`) or an application holding
`root:content:write`:

```go
out, err := client.IngestMedia(ctx, nil, media.RefBody{Kind: "gallery", ID: contentID, Version: versionID},
	operationID, []contentkit.MediaFile{{Path: "originals/0001.png", Type: "image/png", Body: f}})
```

`IngestMedia` presigns each file, uploads it (one PUT or multipart parts) and
commits the folder in one operation; sending the files again (a new
`operationID`) uploads nothing the folder already holds. A file whose `From`
names a key of the media bucket outside ContentKit's namespaces (a legacy
export staged there) is copied server-side instead (`POST
/v1/media/upload/copy`, `root:import:write`), so no byte passes through the
importer. Derivative processing follows upload commit. Publication is separate intent and
waits for required media before making the completed version reader-visible.

Import content and source media first, then write chapter start markers with
`PUT /v1/contents/{id}/versions/{vid}/segments` (`Client.PutContentVersionSegments`).
Each `start_page` is the source file's virtual manifest path; the chapter runs
until the next marker in manifest order. Omit `VersionRecord.segments` from bulk
imports: the chapter route holds the source and recovered manifest locks through
the catalog commit, while an import's caller-owned transaction cannot safely
acquire them. Numeric ranges are accepted only by explicit legacy migration.

Every file lives in a version; versions share none. A legacy record's one
cover is ingested into each version's folder (a `Named` cover upload) and
each `VersionRecord.cover` names that version's copy, sent once the files are
in. Body images go into each version's folder too (its `Body` upload),
named as the body references them.

## Version-owned text

The parent record has shared identity, creators, channel and metadata. Titles,
summaries, bodies, aliases and original-language titles belong to each version.
An import specifies them under `versions[].text[language]`, along with that
version's explicit `external_id`. Existing editions retain their IDs on replay.
New editions start empty unless the import explicitly supplies content; no sibling
text or files are copied automatically. A reader response may expose convenient
selected-version fields, but those are read-only projections.

An English gallery and Chinese gallery edition can have wholly different image
bytes. Shared memberships omit `version`; edition-specific memberships name it.
Each edition's body resolves inline images only from that edition's uploads.

## Record shapes

| Section | Record | Keys |
|---|---|---|
| `entities` | `EntityRecord` | `external_id`, `kind`, `slug`, `scope`, `parent`, `state`, `names`, `text`, `attributes`, `cover_image` / `cover_content`, `avatar`, `edges`, `code`, times |
| `channels` | `ChannelRecord` | `external_id`, `slug`, `state`, `names`, `text`, `cover_image` / `cover_content`, `avatar`, `owner_user_id`, `auth_scope`, `code`, times |
| `contents` | `ContentRecord` | `external_id`, `kind`, `state` (draft, published, deleted), `publish_at`, `published_at`, `deleted_at`, `comments` (open, closed, hidden), `access`, `restrictions`, `attributes`, `channel`, `creators`, `versions`, `code`, times |
| | `VersionRecord` | `external_id`, `languages`, `traits`, `label`, `state`, times, `access`, `attributes`, `ordinal`, `cover`, `original_title`, `original_language`, `text` |
| `memberships` | `MembershipSet` | `content`, `kinds`, `memberships` (`group`, `version`, `role`, `position`, `state`) |
| `aliases` | `AliasRecord` | `kind`, `key`, `locator`, `target` (content, group) |
| `comments` | `CommentRecord` | `external_id`, `target` (content, version), `language`, `reply_to`, `user_id` / `anon_name`, `body`, `deleted_at`, times |
| `reactions` | `ReactionRecord` | `target` (content, version, comment, group), `user_id` / `ip`, `value` |
| `favorites` | `FavoriteRecord` | `target` (content, version), `user_id` |
| `polls` | `PollRecord` | `external_id`, `kind`, `question`, `language`, `active`, `live_at`, `closes_at`, `deleted_at`, `options` |
| `votes` | `VoteRecord` | `poll`, `option`, `user_id` / `ip` |
| `signals` | `SignalRecord` | `target` (content, version), `user_id` / `anon_key`, `type` (click), `event_id`, `revision`, `occurred_at` |
| `legacy_reading` | `LegacyReadingRecord` | `target` (content, version), `user_id` / `anon_key`, `first_read_at`, `last_read_at`, `progress`, `progress_max`, `completed`, `resume`, `revision` |
| `deletes` | `DeleteRecord` | `entity` (content, version, group, comment, poll), `external_id` |

Poll imports support `multiple_choice` only. Free-text polls and the removed
`answers` section are rejected; historical free-text records are not converted.

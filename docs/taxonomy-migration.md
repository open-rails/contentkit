# Taxonomy adoption

The `taxonomy` package replaces each host's duplicated tag/creator/character/
series layer with four generic tables in the host schema plus a derived count
table. Hosts adopt in place, one at a time, under the pre-launch hard-cut rule:
rename or copy their rows into the library tables, keep every id, drop the old
tables and triggers. No compatibility views.

Lineage: `migrations.Taxonomy` (migratekit app id `contentkit_taxonomy`),
applied into the same schema after the keyword profile it depends on
(`contentkit_keyword_normalize`, `content_search_documents`, the dirty queue).

## Tables (31 catalog columns + 6 derived)

| Table | Columns | Key |
|---|---|---|
| `content_nodes` | tenant_id, taxonomy_id, kind, slug, state, source_revision, created_at, updated_at | `(tenant_id, taxonomy_id)`; one active slug per tenant and kind |
| `content_node_names` | tenant_id, taxonomy_id, language, kind (`name`\|`alias`), name, normalized (generated), source_revision, created_at | `(tenant_id, taxonomy_id, language, normalized)`; one `name` per language |
| `content_edges` | tenant_id, from_taxonomy_id, relation, to_taxonomy_id, source_revision, created_at | `(tenant_id, from, relation, to)`; relations `alias_of`, `member_of`, `artist_of`, `voice_of`, `parent`, `child`, `synonym`; "from is *relation* of to" |
| `content_assignments` | tenant_id, content_kind, content_id, content_version_id (nullable), taxonomy_id, relation, source_revision, state (`active`\|`proposed`), created_at | unique on the six key columns with NULL = the work |
| `content_node_counts` (derived) | tenant_id, taxonomy_id, content_kind, language, content_count, updated_at | rebuilt by `RebuildCounts`, recomputed on write |

Every foreign key carries `tenant_id`, so a node can only be named, linked,
assigned or counted inside its own tenant. `taxonomy_id` is opaque text:
bigint and uuid host ids adopt as their text form. Product metadata that is
not a name, alias, edge or assignment (descriptions, `restricted`, creator
`type`, `cover_key`, `indexable_buckets`, `sort_key`) stays in a host sidecar
table keyed by `(tenant_id, taxonomy_id[, language])`.

## Semantics preserved

- **Effective tags** = work assignments ∪ the selected version's assignments,
  deduplicated per `(taxonomy_id, relation)`. `Store.EffectiveTags` returns
  them with the scope; `taxonomy.RequireAll` renders the same rule as a
  `FilterSQL` fragment over the candidate document `sd`, so a multi-node filter
  holds on one version row together with the host's eligibility join. This is
  what `hentai0.effective_video_tags` and the doujins
  `gallery_tags ∪ gallery_version_tags` join expressed; a trait of one edition
  never leaks to a sibling edition or language.
- **Counts** are `count(DISTINCT content_id)` per node, content kind and
  document language over `content_search_documents` joined with
  `Options.CountEligibility` (the host's public-visibility join): a work counts
  once per language across its editions, a trait edition counts its own
  language only, hidden or unreleased versions never count. That is
  `expected_entity_gallery_counts()` and `refresh_effective_tag_counts` without
  triggers. Writes through the store recount the touched nodes in the same
  transaction; `RecountContent` covers host-side version/visibility changes;
  `AssignOptions{SuppressCounts}` replaces the `suppress_entity_counts` GUC for
  bulk loads and `RebuildCounts` replaces `refresh_all_entity_gallery_counts`.
- **Typeahead** documents are built by the store for its kinds (title = the
  language's canonical name, falling back to English then any language;
  aliases = same-language aliases and every other name) and flow through the
  existing dirty queue; hosts wrap their builder with `store.Builder` and their
  lister with `store.Lister`.

## Doujins

| Existing | Becomes |
|---|---|
| `tags(id, slug, deleted_at)` | node kind `tag`, `taxonomy_id = id::text`; `deleted_at` → state `deleted` |
| `tags.display_name`, `tag_i18n.localized_name` | names: `en` canonical from `display_name` unless `tag_i18n` has `en`; every `tag_i18n` row a canonical name in its language |
| `tag_i18n_aliases.alias` | alias in the `tag_i18n` row's language |
| `artists(id, slug, romanized_name, native_name, native_name_lang, type)` | kind `artist`; `en` canonical = romanized_name, `native_name_lang` canonical = native_name; `type` → sidecar |
| `artist_aliases.alias` | `en` alias |
| `characters(id, slug, series_id, …)` | kind `character`; edge `(character, member_of, series)`; slugs are unique per series today, so re-slug collisions as `<series-slug>-<slug>` before the cut |
| `series(id, slug, parent_id, is_categorical)` | kind `series`; edge `(child, child, parent)` for `parent_id`; `is_categorical` → sidecar |
| `voice_actors(id uuid, display_name)` | kind `voice_actor`, `taxonomy_id = id::text`, `en` canonical name |
| `gallery_tags` | assignments `(gallery, gallery_id, NULL, tag_id, 'tag')` |
| `gallery_version_tags` | assignments `(gallery, gallery_id via gallery_versions, version_id, tag_id, 'tag')` |
| `gallery_artists` / `galleries.publisher_id` | relations `artist` / `publisher` to `artist` nodes |
| `gallery_characters`, `gallery_series` | relations `character`, `series` |
| `voice_actor_gallery_versions` | assignments with `content_version_id`, relation `voice_actor` |
| `entity_gallery_counts` + triggers + `refresh_all_entity_gallery_counts` | `content_node_counts`, `RecountContent` on version release/deletion, `RebuildCounts` |

`CountEligibility` for doujins is the released-version join already used for
search (`gv.deleted_at IS NULL AND gv.live_at <= now() AND gv.page_language =
sd.language`), so counts follow the same page-language rule as today.

## Hentai0

| Existing | Becomes |
|---|---|
| `tags`, `tags_i18n.display_name`, `tags_i18n_aliases` | kind `tag`; names per language; `restricted` → sidecar |
| `creators(display_name, type)`, `creators_i18n` | kind `creator`; `en` canonical = display_name; `type`, descriptions → sidecar |
| `characters`, `characters_i18n.localized_name` | kind `character`; edge `member_of` series |
| `series`, `series_i18n.display_name` | kind `series` (the saga/franchise) |
| `seasons`, `seasons_i18n.display_name` | kind `season` (an ordered run); classify rows by meaning before renaming; `cover_key` → sidecar |
| `videos.season_id` | assignment `(video, id, NULL, season_id, 'installment')`; the ordinal stays in the host's episode table |
| `video_tags` (work) / `video_version_tags` | assignments with NULL / the version id, relation `tag` |
| `video_creators.role` | relation = role (`creator` when empty) |
| `video_characters`, `video_series` | relations `character`, `series` |
| `effective_video_tags` view | `EffectiveTags` / `RequireAll` |
| `entity_video_counts`, `refresh_effective_tag_counts`, `entity_counts_suppressed()` | `content_node_counts`, `RecountContent`, `AssignOptions{SuppressCounts}` + `RebuildCounts` |

Hentai0's count languages come from its documents: it indexes one document
per creative version and audio/subtitle language, so `CountEligibility` is the
live-version join and the language rule is unchanged.

## Order of work per host

1. Apply the taxonomy lineage in the host migrate step (after the keyword profile).
2. In one transaction with `AssignOptions{SuppressCounts: true}`: create nodes
   (ids as text), names, edges, assignments from the tables above, keeping
   `source_revision` = the host row's version where one exists.
3. `RebuildCounts`; compare with `expected_entity_gallery_counts()` /
   `entity_video_counts` row for row before dropping them.
4. Register the taxonomy kinds in the worker (`ContentKinds`, `store.Lister`,
   `store.Builder`); run the backfill; compare typeahead goldens.
5. Switch reads (`Node`, `ListNodes`, `EffectiveTags`, `Browse`/`RequireAll`,
   `Counts`) and the admin routes (`taxonomy.Handler`), mark content documents
   dirty in the transactions that call `Assign`/`Unassign`.
6. Drop the old tables, triggers, views and count functions; record the merge,
   counts diff and golden results in the tracker.

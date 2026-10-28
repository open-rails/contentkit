# Shared content filters and domain rules

A domain is a hostname entrypoint into one app's catalog. Aliases can share a
policy; the domain named `main` is the fallback for unmatched hosts,
independent of list order. A configured list requires exactly one `main`; only
`main` may omit `hosts`. An empty list disables domain policies. Preserve the
original Host through trusted proxies. Selecting a domain changes neither the
account tenant nor the caller's authority. `name` is the stable key used for
branding, activity and domain totals; `hosts` groups hostname aliases under
that identity. Renaming it changes the identity, so keep it stable when adding
or removing aliases.

An app has one entity pool and one reader preference per entity. The same tag
can be associated with galleries and blog posts. Another app's catalog and
preferences remain separate, even when the apps share accounts and billing.

## Configuration

ContentKit accepts this directly as part of `kinds.Config`:

```yaml
languages: [en, ja]
content_kinds:
  gallery:
    route: g
    presentation: reader
    versions: {languages: one}
    entities: [tag]
  blog:
    route: blog
    presentation: post
    versions: {languages: one}
    entities: [tag]
entity_kinds:
  tag: {route: tags}
content_filters:
  applies_to: [gallery, blog]
  domains:
    - name: main
      hosts: [catalog.example.test]
      filters:
        - {kind: tag, slug: spoilers, visible: false}
        - {kind: tag, slug: restricted, visible: false, user_toggle: false}
    - name: gay
      hosts: [doujins.gay, www.doujins.gay]
      require_entities:
        gallery: [{kind: tag, slug: yaoi}]
      filters:
        - {kind: tag, slug: furry, visible: true}
```

`applies_to` explicitly names the content kinds sharing the filter layer. Each
must permit the referenced entity kinds in its `entities` list. A filter applies
to every selected kind. Requirements are independent: in this example galleries
must have Yaoi, while an untagged blog post remains eligible.

`name`, `hosts` and `filters` are domain fields; content-kind names occur in
`applies_to` and `require_entities`. There are no dynamic `gallery:` blocks,
per-kind duplicate filter lists or reserved content-kind names.

Embedded apps may keep this policy block in their own root configuration:
`kinds.DecodeContentFilters(data)` accepts an isolated YAML or JSON block and
returns `kinds.ContentFilters`. Assign it to `model.ContentFilters`, then
validate the full model. The decoder is strict even inside a permissive outer
loader: retired per-kind blocks, labels and misspelled fields fail instead of
silently broadening a restricted domain. Entity display names and translations
are not configuration fields. Filter rows are flat `{kind, slug, visible,
user_toggle?}` references in YAML and JSON; the retired `entity: {...}` wrapper
is rejected. The typed Go API continues to use `ContentFilter.Entity` as a
`EntityRef`, so application code retains the same compiler-checked model.

## Visibility

- An entity omitted from the domain's filters is allowed and has no user toggle.
- `visible` is the default for matching content in every `applies_to` kind.
- `user_toggle` defaults to true. Internal or required entities never become
  optional controls, regardless of this setting.
- `visible: false, user_toggle: false` is a hard exclusion. Matching content
  cannot be listed, searched, opened directly or newly authorized as media on
  that domain, including through public staff reads.
- `visible: true, user_toggle: false` permits matching content without a toggle;
  it does not require the entity.
- Every reference in `require_entities.<kind>` must be associated with the item.
  Requirements combine with AND and affect only that named content kind.
- Optional reader hiding affects discovery, including public staff browsing.
  Direct links and explicitly requested entity pages ignore optional hiding.
  Authorized management operations retain their management visibility.

Invalid or repeated content/entity references and contradictory required/hidden
rules fail validation. Missing requirements fail closed for their named kinds;
missing fixed exclusions fail closed for every `applies_to` kind. Missing
optional filters are omitted. Unselected kinds remain unaffected by the shared
filter list.

Hard rules deny new media authorizations through the app. The stock gateway
checks signed capabilities without reading the catalog: previously issued
private URLs remain valid until expiry, and public previews remain public.
Domain filtering does not instantly revoke issued media capabilities.

## Names and internal entities

References use canonical entity kind and slug. Localized display names and
aliases belong to entity records; the reader UI uses their normal language
fallback. No policy labels duplicate those names.

An internal entity remains available to authorized management, imports and
publishing, but is absent from public lookup, lists, autocomplete, search
metadata and filter controls. Marking an entity internal does not, by itself,
hide its associated content. A domain can fix an internal `restricted` tag to
hidden while another domain allows its content. Its names stay private on both.

Policy-bound hard references cannot be renamed, deactivated or made public in a
way that would weaken a rule; change the policy before such mutations. Inactive
internal records retain the evidence used by fail-closed resolution.

## Reader interface

`GET /v1/content-filters` returns one flat inventory:

```json
{"applies_to":["gallery","blog"],"filters":[{"entity":{"id":"<id>","slug":"spoilers","name":"Spoilers"},"default_visible":false,"visible":false,"overridden":false}]}
```

Only public controls offered on the current domain appear. Fixed rules,
requirements and internal references are not exposed through `/v1/kinds`.

`PATCH /v1/me/content-filters` changes only submitted choices:

```json
{"filters":[{"entity_id":"<id>","visible":true}]}
```

Omitted entities remain unchanged. The JSON merge is
atomic, so independent edits from two tabs do not replace each other's choices.
A reset is explicit and follows the current domain default:

```json
{"filters":[{"entity_id":"<id>","default":true}]}
```

Exactly one of `visible` or `default: true` is required for each choice.
Preferences never override fixed rules, requirements or entities not offered on
that domain. An explicit choice is shared by all applicable content kinds and
by other domains offering that entity; omitted entities remain unfiltered.

Daily release summaries count unique omitted content and aggregate each hidden
entity over the actual selected content kinds. Entity counts can overlap, so
their sum is not the total. Omitted content identities and covers remain private.

Guests keep shared choices in app-scoped localStorage and send a bounded
`ContentKit-Reader-Preferences: {"filters":[{"entity_id":"<id>","visible":false}]}`
header. It grants no access and cannot select a domain. Signed-in requests use
the account's database settings instead, ignoring the guest header.

After confirmed registration, `POST /v1/me/content-filters/initialize` adopts
valid guest choices only when the authenticated account has no saved preference
row. `account_id` must match the authenticated account; retries cannot overwrite
saved choices. Initialization validates against all the app's domains and
ignores stale, internal or never-toggleable entities. Ordinary login never
imports guest preferences. Language remains a separate account preference.

## Existing preferences and cutover

No account data or catalog schema is reset. Database reads understand existing
values without mutating them. New writes use `entity:<id>` keys in the existing
catalog-scoped preference row:

1. A new canonical value wins all legacy values, including a canonical reset.
2. Among old per-kind explicit choices, hidden wins conflicting shown choices.
3. A scoped default suppresses an older unscoped fallback when no explicit
   per-kind choice remains.
4. The old unscoped value is used only when no scoped choice exists.

The browser migrates its v1 document into a v2 storage key once, including
language and pending registration snapshots. The original v1 bytes remain for
rollback; old tabs cannot overwrite new v2 choices. Subsequent edits read the
current document and preserve omitted entities. Registration retries retain
account identity and completion markers.

Deploy the matching core and UI release with updated application policy loaders.
The retired kind-grouped wire format and dynamic per-kind YAML are intentionally
rejected. Old browser tabs need a refresh; accounts, sessions and enrollment data
are preserved. This change does not alter media job formats or require another
media-worker rollout solely for filter flattening.

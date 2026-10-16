# Domain content rules and reader filters

A domain is a hostname entrypoint into one app's catalog. Host aliases can share
one policy. The first domain is the fallback for unknown hosts and in-process
calls without a request hostname. Hosts and trusted reverse proxies must preserve
the original Host; choosing a domain never changes account tenancy or grants.

Each domain declares its own filters. There is no global available-filter list:

```yaml
languages: [en, ja]
kinds:
  gallery:
    route: g
    presentation: reader
    versions: {languages: one}
    groups: [tag]
group_kinds:
  tag: {route: tags}
domains:
  - name: main
    hosts: [catalog.example.test]
    filters:
      - entity: {kind: tag, slug: spoilers}
        visible: false
      - entity: {kind: tag, slug: restricted}
        visible: false
        user_toggle: false
  - name: gay
    hosts: [doujins.gay, www.doujins.gay]
    require_kinds: [gallery]
    require_entities:
      - {kind: tag, slug: yaoi}
    filters:
      - entity: {kind: tag, slug: gore}
        visible: false
```

Applications may load this policy from their own configuration and pass it to
ContentKit; Doujins and Hentai0 use `content_filters.domains` in `config.yaml`
and `config.example.yaml`. Structural content kinds remain in the content model.

## Meaning of each rule

- An entity omitted from a domain's filters is allowed and has no user toggle.
- `visible` is the default visibility of content associated with the entity.
- `user_toggle` defaults to true. A reader may change an optional filter.
- `visible: false` with `user_toggle: false` is a hard exclusion: matching
  content cannot be listed, searched, opened directly or served as media on
  that domain, including to a signed-in staff account through public routes.
- `visible: true` with `user_toggle: false` allows the content and offers no
  toggle. It does not require content to have that entity.
- Every entry under `require_entities` must be associated with a content item
  for the item to be accessible. Optional `require_kinds` limits these
  requirements to named content kinds; omit it to cover every kind. Required
  entities are not reader toggles. Requiring Yaoi for galleries excludes every
  gallery without Yaoi on Doujins.gay while keeping blog posts available.

A soft hidden filter affects ordinary listings and search, not direct links or
an explicitly requested entity page. Hard rules always apply. Contradictory
required/hidden rules and invalid references must be rejected at configuration
validation rather than silently broadening access.

## Entity names and internal entities

Create referenced entities before enabling hard rules. An unresolved hard
exclusion or requirement fails closed; missing optional filters are omitted.

References use stable entity kind and slug. Display names, translated names and
aliases belong to the entity record, never the filter policy. The settings UI
uses the entity's localized display name and normal language fallback.

An entity marked `internal` is available to authorized management APIs, imports
and publishing, but is absent from public entity lookup, lists, search,
autocomplete and content metadata. Its names must not contribute to public
content search documents. Existing identity and alias resolution still reuse
it during authorized publishing.

Internal entities are never user-toggleable, even if a rule requests it.
Consequently a hidden internal entity is a fixed domain exclusion. Hentai0 can
use an internal `restricted` entity hidden on hmovie-moe.us and allowed on
hentai0.com, while its label remains invisible on both public sites.

## Reader preferences

`GET /v1/content-filters` is public and returns only the current domain's
available public toggles, with localized entities and effective/default values.
Hard policy and internal entity references are not exposed through `/v1/kinds`.

Signed-in readers save choices using `PUT /v1/me/content-filters`. Replacing the
current domain's choices must preserve choices that belong to other domains.
Preferences apply only to currently offered toggles; they cannot override fixed
rules, requirements or omitted entities.

Guests keep choices in localStorage and send only applicable overrides in
`ContentKit-Reader-Preferences`. The bounded header never grants access or
changes the host-selected domain. Stale entries cannot weaken current policy.
Authenticated requests use account preferences instead of the guest header.

After registration, `POST /v1/me/content-filters/initialize` adopts the guest's
choices only when the authenticated account has no saved preferences. This
includes valid choices saved for other domains of the same app; stale or
unknown entities are discarded. An existing account's login never overwrites
its preferences with guest state.
Both server and SDK preserve choices across domains while honoring each
domain's current controls. Language remains a separate account preference.

## Upgrading an existing reader deployment

Apply the schema migration before starting this version. Readers and media workers from before
internal entities were introduced do not understand that flag: drain them
before activating internal markers or converting public entities to
internal. A host backfill must retain any old restriction flags while an old
reader still serves requests. Normal task startup can migrate after stopping
the old stack; a live switchover needs the schema first and marker activation
after the reader change.

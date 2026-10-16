# Domain content rules and reader filters

A domain is a hostname entrypoint into one app's catalog. Host aliases can share
one policy. The first domain is the fallback for unknown hosts and in-process
calls without a request hostname. Hosts and trusted reverse proxies must preserve
the original Host; choosing a domain never changes account tenancy or grants.

Every domain scopes its requirements and filters by content kind:

```yaml
languages: [en, ja]
kinds:
  gallery:
    route: g
    presentation: reader
    versions: {languages: one}
    groups: [tag]
  blog:
    route: blog
    presentation: post
    versions: {languages: one}
group_kinds:
  tag: {route: tags}
domains:
  - name: main
    hosts: [catalog.example.test]
    gallery:
      filters:
        - {entity: {kind: tag, slug: spoilers}, visible: false}
        - {entity: {kind: tag, slug: restricted}, visible: false, user_toggle: false}
  - name: gay
    hosts: [doujins.gay, www.doujins.gay]
    gallery:
      require_entities: [{kind: tag, slug: yaoi}]
      filters:
        - {entity: {kind: tag, slug: yuri}, visible: false}
        - {entity: {kind: tag, slug: futanari}, visible: false}
        - {entity: {kind: tag, slug: furry}, visible: false}
```

The top-level `kinds` defines the content model. Within each domain, content
kind names such as `gallery` directly contain that kind's policy; no inner
`kinds` wrapper is needed. A kind omitted from a domain is unfiltered:
these gallery rules do not affect blog posts. There is no global filter list
or separate `require_kinds` switch.

`name` and `hosts` are reserved for domain metadata and cannot be content kind
names. ContentKit rejects them during configuration validation.

Applications may load policy from their own configuration and pass it to
ContentKit. Doujins and Hentai0 use `content_filters.domains` in `config.yaml`
and `config.example.yaml`; their structural content models stay separate.

## Rules within a content kind

- An entity omitted from that kind's filters is allowed and has no user toggle.
- `visible` is the default visibility of matching content of that kind.
- `user_toggle` defaults to true. A reader may change an optional filter.
- `visible: false` with `user_toggle: false` is a hard exclusion: matching
  content cannot be listed, searched, opened directly or served as media on
  that domain, including to staff using public routes.
- `visible: true` with `user_toggle: false` permits matching content without a
  toggle. It does not require content to have that entity.
- Every `require_entities` entry must be associated with the content item.
  Requirements combine with AND and are never reader toggles. Requiring Yaoi
  under `gallery` excludes non-Yaoi galleries while leaving blogs unaffected.

Soft hidden filters affect ordinary discovery, not direct links or an
explicitly requested entity page. Hard rules apply to direct access too.
Contradictory required/hidden rules and invalid content/entity kinds fail
configuration validation. Mixed-kind queries apply each item's own policy.

Hard rules also deny new media authorizations through the app. The stock media
gateway validates signed capabilities without reading the catalog: previously
issued private-media URLs remain valid until expiry, and public preview assets
remain public. Domain filtering is not instant revocation of already issued
media capabilities.

## Entity names and internal entities

Create referenced entities before enabling hard rules. An unresolved hard
exclusion or requirement fails closed for its content kind; missing optional
filters are omitted.

References use stable entity kind and slug, written directly in configuration.
Display names, translated names and aliases belong to the entity record. The
settings UI uses those names and normal language fallback.

An entity marked `internal` is available to authorized management APIs, imports
and publishing, but is absent from public lookup, lists, search, autocomplete
and content metadata across all content kinds. Its names do not contribute to
public search documents. Authorized publishing still resolves its identity and
aliases.

Internal entities never become user toggles. Hentai0 can fix an internal
`restricted` entity hidden under `video` on hmovie-moe.us and allow it on
hentai0.com. The entity's metadata remains private on both domains; marking an
entity internal alone does not hide its associated content everywhere.

## Reader preferences

`GET /v1/content-filters` returns the current domain's available public toggles,
grouped by content kind in `kinds`, with localized entities and effective/default
values. Hard policy and internal references are not exposed through `/v1/kinds`.

Signed-in readers save choices through `PUT /v1/me/content-filters`:

```json
{"kinds":{"gallery":[{"entity_id":"<entity-id>","visible":false}]},"include_deleted":false}
```

Only the kinds present in the update are replaced, and only their currently
toggleable domain choices are affected. Omitted kinds and other domains are
preserved. An empty kind array resets that kind's current-domain choices.
The same entity can be hidden for galleries and shown for another content kind.
Preferences cannot override fixed rules, requirements or unconfigured entities.

Guests keep the same scoped choices in localStorage and send a bounded
`ContentKit-Reader-Preferences` header. The header does not grant access or
choose the domain. Signed-in requests use account preferences instead.

After registration, `POST /v1/me/content-filters/initialize` adopts valid guest
choices only when the authenticated account has no saved preferences. It
validates choices across all the app's domains and kinds. Ordinary login never
overwrites an account with guest preferences. Language remains a separate
account preference.

Existing unscoped browser and database choices are retained as a fallback for
configured kinds. Explicit scoped values win. Resetting a kind records a
scoped default so an older unscoped value cannot unexpectedly reappear.

## Upgrading an existing deployment

Replace domain-level `filters`, `require_entities` and `require_kinds` with
policies directly under `domains[]`, keyed by content kind (for example,
`domains[].gallery`). Apply schema migrations before starting a
new reader. Readers and media workers from before internal entities were
introduced do not understand that flag: drain them before activating internal
markers or converting public entities to internal. A host backfill must retain
old restriction flags while an old reader still serves requests.

Normal task startup can migrate after stopping the old stack. A live cutover
requires schema preparation and coordinated reader/worker replacement before
internal marker activation; see the host's production rollout instructions.

# Content filters and entrypoint defaults

Content filters are an app's configurable reading preferences. They hide tagged
content from ordinary discovery lists and searches; they do not revoke access
or prevent opening a direct content URL. A tag's own page can still show its
explicitly requested content. Actual access rules, `require_groups` and
`exclude_restrictions` continue to apply independently.

Declare filters in the content model (`Config.Content`). Each filter names an
active, unscoped group by kind and slug. The groups must already exist: create
them through ContentKit's group API or an explicit import/setup command.
Declaring a filter does not create groups, seed content or run a database
migration. The settings UI omits filters whose groups do not exist.

```yaml
languages: [en]
kinds:
  gallery:
    route: g
    presentation: reader
    versions: {languages: one}
    groups: [tag]
group_kinds:
  tag: {route: tags}
content_filters:
  - group: {kind: tag, slug: spoilers}
    default_visible: false
    labels: {en: Spoilers}
  - group: {kind: tag, slug: graphic}
    default_visible: true
    labels: {en: Graphic scenes}
sites:
  - name: main
    hosts: [catalog.example.test]
  - name: alternate
    hosts: [alternate.example.test]
    content_filter_defaults:
      - {group: {kind: tag, slug: spoilers}, visible: true}
      - {group: {kind: tag, slug: graphic}, visible: false}
```

An unspecified site default inherits the app's `default_visible`. An explicit
`visible: false` overrides an app default of `true`. Filter references and site
names must be unique; a site default must name a declared content filter.
Host matching ignores case and port. The first site is the fallback for
unmatched hosts and for in-process calls without a request host.

For an ordinary reader, visibility is resolved in this order:

1. The app's default.
2. The matched site's override, if any.
3. The reader's explicit saved override, if any.

Saved overrides belong to the account within this app, so an explicit choice
continues to win when that person visits another entrypoint of the same app.
It does not become a preference in another app sharing the tenant account.
Anonymous readers have only the resolved site defaults.

`GET /v1/me/content-filters` returns the current site's `default_visible`, the
effective `visible` value and whether it was `overridden`. Saving uses
`PUT /v1/me/content-filters`; its `filters` list replaces the saved overrides.
Omit a filter from that list to reset it to the current site's default. For
example, the following resets all filter overrides and disables the separate
include-deleted preference:

```json
{"filters": [], "include_deleted": false}
```

Lists, searches and recommendations share the catalog's resolved preferences.
Public catalog totals use site defaults, independent of any reader's saved
overrides, and cache separately by site. Direct URLs and sitemaps retain their
existing access/site rules; soft preferences do not make content private.

The server derives the entrypoint from the request host already bound by its
route gate. Hosts and trusted development/reverse proxies must preserve the
original `Host` when routing these requests to the app. Do not add a query
parameter or arbitrary forwarded header that changes this selection. The app's
tenant/app authorization binding stays fixed: choosing a hostname never
selects a different tenant or grants permissions.

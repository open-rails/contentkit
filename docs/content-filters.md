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

Filter names come from the entity's localized display name, with the same language fallback used on
entity cards. Keep translations on the entity; filter configuration only
selects entities and visibility defaults.

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
  - group: {kind: tag, slug: graphic}
    default_visible: true
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
3. The reader's explicit override, saved to their account or supplied by an anonymous browser.

Saved overrides belong to the account within this app, so an explicit choice
continues to win when that person visits another entrypoint of the same app.
It does not become a preference in another app sharing the tenant account.
Anonymous readers can keep choices in local storage and send them with each
request in the `ContentKit-Reader-Preferences` header:

```json
{"filters": [{"group_id": "configured-entity-id", "visible": false}]}
```

The header changes only soft content filters. It grants no access, does not
change the entrypoint's restrictions, and never writes an anonymous account
row. It is limited to 8 KiB and 128 filters; malformed values, duplicate IDs
and IDs that are not configured filters return `400 invalid_request`.
Authenticated readers always use their account settings, even when a request
carries the header. Hosts must allow this header in their CORS policy.

`GET /v1/content-filters` is public and returns the current site's `default_visible`, the
effective `visible` value and whether it was `overridden`. Saving uses
`PUT /v1/me/content-filters`; its `filters` list replaces the saved overrides.
Omit a filter from that list to reset it to the current site's default. For
example, the following resets all filter overrides and disables the separate
include-deleted preference:

```json
{"filters": [], "include_deleted": false}
```

After a new account registers, `POST /v1/me/content-filters/initialize` can
adopt its guest choices. Its body includes `account_id`, `filters` and
`include_deleted`. The account ID must match the authenticated account;
mismatches return `403 forbidden`. Initialization inserts only if no saved
settings exist, so retries and delayed callbacks cannot replace account
choices, including an explicit reset to defaults. Ordinary sign-in must load
account preferences instead of initializing them from the browser.

Before attaching persisted guest choices, fetch `GET /v1/content-filters`
without the header and discard choices whose IDs are no longer configured.
This keeps old browser storage from making catalog requests invalid after a
filter is removed. Language remains a separate account profile preference;
hosts can persist guest language in the same browser store and adopt it on
registration through their account profile adapter.

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

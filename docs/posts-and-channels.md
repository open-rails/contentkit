# Posts and channels

A post is a normal ContentKit content item, with its title/body, creator credits,
comments, reactions, visibility and one or more media versions. A channel is an
entity that owns posts. Channel ownership does not require paid access.

Hosts configure the same primitives for different products:

| Product | Channel management | Posting/access policy |
| --- | --- | --- |
| Official site blog | Site staff | Staff publish; every post is public/free |
| Creator feed | Creator and assigned editors | Creator publishes; public or entitlement-locked posts |
| Community | Community moderators | Host decides who may submit/edit their own posts and who moderates others |

The host's frontend can display a blog, timeline or community feed. Creation,
editing, moderation and reading are separate policy decisions; granting someone
permission to contribute must not implicitly grant editing of everyone else's
posts. Hosts must declare and enforce the permissions appropriate to their
product. ContentKit does not manufacture a universal social-network role bundle.

OpenRails owns channel subscription products, purchases, renewals and entitlement
expiry. ContentKit asks whether the viewer has the required entitlement when
serving a protected post. Expiration needs no ContentKit collection timer.

## One fixed official channel

A site blog can fix a post kind to a site-owned channel:

```yaml
kinds:
  blog:
    route: blog
    presentation: post
    versions: {languages: one, hidden: true}
    access_levels: [public]
    owner_entity: {kind: channel, slug: blog}
entity_kinds:
  channel: {route: channels, owner: site}
```

This channel has no user owner, delegated editor scope or billing entitlement.
Normal app content editors manage the posts. `owner_entity` is enforced by the
server for ordinary creation and imports: an omitted owner is filled, and a
conflicting owner is rejected. A channel in another app cannot satisfy the
reference, even with an identical slug. The referenced channel must be seeded
explicitly; startup never creates it.

After schema migrations, with writers stopped, the host migration command calls:

```go
changed, err := contentkit.MigrateFixedOwners(ctx, pool, cfg, []entities.EntityInput{{
    Kind: "channel", Slug: "blog",
    Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official blog"}},
}})
```

The helper uses the same normalized app scope/schema as `MigratePostgres`, without
starting AuthKit, billing, media or workers. It creates an absent configured
channel and binds unowned existing posts atomically. Existing names, titles,
author credits and access rules remain unchanged. A conflicting channel owner or
a post/version access rule outside current policy rejects the transaction; the
operator must resolve that explicitly. Repeating the migration is harmless.

Omit `owner_entity` for creator-selected channels, as OnlyDemo does. Channel
ownership and optional editor teams then use the existing channel permissions.

## Per-post attachment limits

The existing media configuration already expresses independent image/video limits.
For example, a host can configure a post media kind with:

```go
Uploads: []media.Upload{
    {Path: "images/{name}", Types: []string{"image/jpeg", "image/png", "image/webp"},
        Max: 5, MaxBytes: 20 << 20},
    {Path: "videos/{name}", Types: []string{"video/mp4", "video/webm"},
        Max: 1, MaxBytes: 200 << 20},
}
```

These are example values, not defaults for Doujins, Hentai0 or OnlyDemo. OnlyDemo
currently configures its own limits (40 images and 10 videos). A post with one
hidden version has one attachment set; explicitly versioned content has one set
per version. `Max: 0` is unlimited within ContentKit's general resource bounds.

Presign checks the path, declared type and bytes. Commit enforces counts against
the authoritative manifest while holding its serialization boundary, including
concurrent application instances. Copy and archive expansion use the same
upload rules. Processing validates source bytes and builds configured renditions;
derivatives do not count as user attachments. A presigned URL is permission to
stage bytes, not a reservation that bypasses the final attachment limit.

ContentKit UI consumes these media upload rules; the server remains authoritative
for HTTP, Go SDK, imports and custom frontends. A host need not reimplement upload
validation to build a different posting interface.

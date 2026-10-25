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

## Host-defined channel roles

With `auth_scope: true`, each channel has a distinct AuthKit group in the
`channel` persona. The adapter derives an app-isolated group identity, so a role
in one channel or app grants nothing in another. AuthKit stores role assignments;
ContentKit stores the group reference, not a copy of its permissions.

The host declares capabilities and chooses its own role bundles:

| Capability | Meaning in that channel |
| --- | --- |
| `channel:content:create` | Create one's own post and read it; does not grant editing |
| `channel:content:edit-own` | Read, edit, publish and manage media of posts the account actually created |
| `channel:content:write` | Create/edit/publish/delete any post in the channel |
| `channel:settings:manage` | Manage the channel itself |
| `channel:media:write` | Manage the channel's cover/avatar, not arbitrary post media |
| `channel:comments:moderate` | Moderate channel comments |

For example, a contributor role can bundle create and edit-own, an editor can
hold content:write, and a moderator can hold comments:moderate. The host decides
which memberships to assign. Public subscriptions and billing entitlements do
not automatically grant any of these authoring permissions. A scoped owner's
permissions come from their current AuthKit owner role, not an owner-ID shortcut.

Own-post rights require immutable `created_by` plus a current grant in the
appropriate group. Editable author credits remain attribution. Without a channel,
the corresponding app-level create/edit-own permissions apply. An app-level
contributor cannot post into an arbitrary channel. Permission checks use the
current credential and grants on each request; revocation does not wait for a
worker. Read-only staff access does not grant upload or editing authority.
Personal account comment bans apply to unowned personal posts. Channel posts use
the channel's moderation scope, so an author's personal block cannot govern a
community or official blog.

The optional channel lifecycle job provisions/retires the AuthKit group and
handles channel deletion/restoration. It is exact durable work triggered by a
change, not permission copying or periodic reconciliation. Unscoped site blogs
may deliberately rely on their existing app staff roles instead.

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

This example channel has no user owner, delegated editor scope or billing
entitlement; normal app content editors manage its posts. Those are host policy
choices. A fixed site channel may instead use `auth_scope: true` and host-defined
AuthKit roles, or `entitled: true` with an appropriate post access policy.
`owner_entity` chooses a destination; it does not grant permissions or force
public access. It is enforced by the
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

The database-only helper can create channels without an authorization scope.
For `auth_scope: true`, provision the channel through the normal AuthKit-backed
entity API first, then call the helper to bind existing posts. The helper refuses
a missing or unprovisioned scope; it never invents permissions or boots AuthKit.

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

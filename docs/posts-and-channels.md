# Posts and channels

A post is a normal ContentKit content item, with its title/body, creator credits,
comments, reactions, visibility and one or more versions. A channel is the
authoring home of the overall item. It is a distinct concept from descriptive
entities such as tags, artists or series. Channel ownership does not require paid
access. English and Chinese editions of one post share its primary channel and
editorial authority; each version retains its own text, media and publication.

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

Each channel has a distinct AuthKit group in the
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
Changing a channel's owner attribution does not transfer AuthKit roles: the host
must explicitly grant/revoke the corresponding group roles. A required scope that
has not been provisioned grants no authority while its lifecycle work retries.

Own-post rights require immutable `created_by` plus a current grant in the
appropriate group. Editable author credits remain attribution. Without a channel,
the corresponding app-level create/edit-own permissions apply. An app-level
contributor cannot post into an arbitrary channel. Permission checks use the
current credential and grants on each request; revocation does not wait for a
worker. Read-only staff access does not grant upload or editing authority.
Personal account comment bans apply to unowned personal posts. Channel posts use
the channel's moderation scope, so an author's personal block cannot govern a
community or official blog.

The channel lifecycle job provisions/retires the AuthKit group and handles
channel deletion/restoration. It is exact durable work triggered by a change,
not permission copying or periodic reconciliation. App staff retain their normal
authority; a dedicated blog-editor role can grant only one channel's capabilities.

## One fixed official channel

Doujins and Hentai0 each declare one internal staff blog. The channel needs no
public route or selector; the posts retain their normal blog URLs:

```yaml
channels:
  creation: staff
content_kinds:
  blog:
    route: blog
    presentation: post
    versions: {languages: one, hidden: true}
    access_levels: [public]
    channel: {fixed: blog}
entity_kinds:
  tag: {}
```

`channel.fixed` chooses a required destination; it does not grant users roles or
force public access. Here `access_levels: [public]` separately makes the blog free.
An omitted channel is filled on creation/import; a conflicting channel is
rejected. Another app's identically named channel does not satisfy the reference.

The normal explicit migration call creates missing configured channels and their
AuthKit groups. Pass the same app-bound scope provider used at runtime, or the
narrow migration adapter using the host's existing AuthKit configuration:

```go
scopes, err := ckauth.MigrationScopes(pool, authConfig, *cfg.Scope)
if err != nil { return err }
err = contentkit.MigratePostgres(ctx, pool, cfg, contentkit.MigrationOptions{Scopes: scopes})
```

AuthKit storage must already be migrated. There is no second AuthKit runtime,
extra seed command, signing-key setup or per-site bootstrap function. Ordinary
`New`/`Start` does not provision these declarations. Assigning actual editors
remains an explicit authorization operation, not configuration.

With writers stopped, migration validates existing content ownership/access,
reserves stable channel IDs, ensures the groups without holding a catalog
transaction, then rechecks and attaches previously unowned items. A lost provider
response is retried with the same IDs. Concurrent migrations converge. Failure
leaves incomplete channels unavailable for authoring; repeat the normal migrate
command to finish. A deleted/conflicting channel or group, foreign app provider,
or incompatible existing post policy is an error, never permission to transfer
someone else's content or make it public.

Names, URLs, AuthKit groups/grants, media and billing identity survive adoption.
New fixed channels use their slug as the initial name; existing display names
are retained. The previous `owner_entity` and generic metadata-authority config
are replaced by the channel declaration.

## Creator channels and descriptive metadata

An app such as OnlyDemo can permit users to create channels and require each
post to name one:

```yaml
channels:
  creation: users
  route: channels
  entitled: true
  entities: [tag]
content_kinds:
  post:
    route: posts
    presentation: post
    versions: {languages: one}
    channel: {required: true}
entity_kinds:
  tag: {}
```

`entitled` enables channel membership as a read entitlement. It does not lock
all posts: the overall post still chooses its public/member access policy.
OpenRails owns the products, prices and grants. AuthKit editor membership is
separate from a reader's paid membership.

A content kind without `channel` has no owning channel. An empty channel policy
allows a channel but does not require one. Channel-free galleries retain their
configured version-specific access behavior. Channel-bearing content uses one
parent access policy across its versions; a version must not weaken it.

Tags on a channel describe the channel. Tags on an item or version describe that
content. None of those associations grants authoring rights or changes its
primary channel. Channel operations use `/channels`; metadata operations use
`/entities`. A channel cannot be managed by treating it as an ordinary entity.

## Reposts

A repost is an idempotent reference from a destination channel to an existing
overall item. The actor must have permission to publish in that destination.
The original channel remains the only authoring home; the destination's editors
cannot edit the original or its versions merely because they reposted it.

The destination feed resolves the original through its normal publication and
read checks. A repost cannot expose a draft, revive a withdrawn/deleted item,
or bypass a subscription or version entitlement. It uses the original media,
comments, reactions and version selection, with no duplicated content or new
media job. Original withdrawal applies on reads without fan-out work.

Removing a repost only removes the destination reference. Repeated reposts and
ownership/repost overlap do not produce duplicate cards for the same original.
This first implementation is limited to one app; cross-app/tenant reposts,
quoted commentary and co-author invitations are separate features.

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

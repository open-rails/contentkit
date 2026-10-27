# ContentKit

ContentKit organizes a business's shared accounts and the content and activity
of its apps. These terms describe ownership, independently of deployment.

## Organizations and people

**Tenant**:
The business or publisher that owns a set of apps and their shared account
directory. A tenant can have one app or several.
_Avoid_: Site, namespace, merchant when referring to ContentKit ownership

**App**:
A tenant-owned product with its own content, activity, preferences,
roles and restrictions. Doujins and Hentai0 are two apps of one tenant.
_Avoid_: Tenant when referring to a site or its content

**Account**:
A person's registration with a tenant, usable across all of that tenant's
apps. An account has a shared profile, avatar and notification inbox.
_Avoid_: Per-app user, app membership

**Registration**:
The creation of an account with a tenant. Visiting another app of the same
tenant does not require another registration.

**App role**:
Authority an account holds in one app, such as moderator or editor. The same
account may hold different roles in different apps.

**App restriction**:
A limit on an account's activity in one app, such as a commenting ban. An
app restriction does not by itself disable the tenant account.

## Content and access

**Domain**:
A hostname through which readers enter an app's content, with requirements and
reader filters scoped to each content kind. Aliases may share one domain policy.
_Avoid_: Site when referring to hostname-specific content policy

**Domain requirement**:
An entity that every item of a configured content kind must be associated with
on a domain, regardless of reader preferences. Other content kinds are unaffected.

**Reader filter**:
A domain-offered choice to show or hide one content kind associated with an
entity. A domain may fix that choice instead of allowing readers to change it.
Preferences distinguish the content kind and entity, so hiding a tag in one
kind does not hide it in another.

**Internal entity**:
An entity used for content administration or access policy whose identity,
names and associations are absent from public discovery and presentation.

**Content**:
An app's complete set of items, such as posts, galleries and videos, together
with their versions, entities and publication rules. Sharing accounts does not
combine the apps' content.
_Avoid_: Catalog, which refers to OpenRails products and prices

**Entity**:
An app-owned record associated with content, such as an artist, series,
character, tag or collection. Entities describe content items, versions or
channels. Multiple records can refer to one entity; the association grants no
editing authority or channel membership.
_Avoid_: Group when referring to ContentKit content associations

**Entity kind**:
A configured type of entity, defining whether a record is an artist, series,
character, tag or another app-specific concept.

**Permission group**:
An AuthKit organization or team in which people hold roles and permissions.
A channel has a permission group for its authors and editors. The group holds
current authorization grants; the channel holds its public identity and posts.

**Content item**:
The shared identity to which versions and inherited metadata belong, with at
most one primary channel. It stores no authored text or media. Its versions
share that channel's editorial authority and parent access policy.

**Version**:
One edition of an item, such as an English or Chinese post. All authored content
(title, summary, body/HTML, original title and media) belongs to its version.
Versions retain independent language, files and publication state. Channel-free content may
also use the application's configured per-version access policy.

**Channel**:
The authoring home of content items, with its own public identity and AuthKit
permission group. A site blog, creator feed or community uses the same channel
model with different host-defined policies. A channel is not a metadata entity.

**Primary channel**:
The single channel that owns an item's authoring context. It is associated with
the overall item, not separately with each version. Descriptive tags and author
credits cannot transfer that authority.

**Repost**:
A reference showing an existing item in another channel. The destination may
remove its reference but gains no editing rights over the original. Original
publication/access checks, versions, comments and reactions remain authoritative.

**Association**:
A link between content and an entity. An association reuses the entity's
identity instead of copying its name into every content item.

**Alias**:
Another known name for the same entity. An alias identifies that entity;
it does not create a second record.

**Parent scope**:
The entity that disambiguates a scoped entity, such as the series to which
a character belongs. The same character name can occur in different series.

**Audience**:
The readers an access policy names: everyone, or holders of particular tier
or group entitlements.

**Audience release**:
The opening of published content to an additional audience at a stated time.
Earlier audiences keep access. A release does not grant a billing entitlement.

**Media upload**:
Files attached to a content version or another media owner. Accepting an upload
starts media processing; it does not publish the content.

**Media processing**:
Durable work that validates and extracts uploaded archives or generates image,
and video renditions. Standalone audio uploads are playable without conversion.
Its progress belongs to the media owner.

**Ownership**:
An account's relationship to content it created or controls in a particular
app. Ownership in one app does not confer authority in another.

**Entitlement**:
A right granted and tracked by OpenRails that ContentKit requires to access a
feature or content. A product can grant rights in one app or several without
merging their content.

**Product**:
An OpenRails catalog item that grants one or more entitlements when acquired.
Products can grant access to specific content, a version, a channel or a site tier.
_Avoid_: ContentKit offer

**Price**:
OpenRails terms for buying a product, including amount, currency and any
recurring billing or access duration. ContentKit does not maintain a second
copy of those terms.

**Access requirement**:
The entitlement or audience condition ContentKit evaluates before granting
access to content. Changing a price does not change that requirement.

**Merchant**:
The OpenRails billing business linked to a ContentKit tenant. The merchant
owns billing facts; it is distinct from the ContentKit tenant.

**Access tenure**:
The elapsed time during which an account held a particular entitlement,
including retained paid access after cancellation and excluding future access.

## Notifications

**Inbox**:
An account's shared collection of notification receipts. An app displays
tenant-wide notifications together with its own app-specific notifications.

**Receipt**:
An account's read and archive state for a notification. Reading a receipt does
not resolve the event or problem that caused it.

**Notification origin**:
The producer and stable source event behind a notification. The origin
supports provenance and replay without creating another receipt.

**Access ended**:
An entitlement is no longer available because it expired or was revoked.
Stopping automatic renewal alone does not mean access ended.

**Applied downgrade**:
A previously scheduled reduction in entitlements that has taken effect.
Scheduling the change and applying it are different events.

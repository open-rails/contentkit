# ContentKit

ContentKit organizes a business's shared accounts and the content and activity
of its apps. These terms describe ownership, independently of deployment.

## Organizations and people

**Tenant**:
The business or publisher that owns a set of apps and their shared account
directory. A tenant can have one app or several.
_Avoid_: Site, namespace, merchant when referring to ContentKit ownership

**App**:
A tenant-owned product with its own catalog, content, activity, preferences,
roles and restrictions. Doujins and Hentai0 are two apps of one tenant.
_Avoid_: Tenant when referring to a site or its catalog

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

**Catalog**:
An app's collection of content, versions, groups, taxonomy and publication
rules. Sharing accounts does not combine catalogs.

**Ownership**:
An account's relationship to content it created or controls in a particular
app. Ownership in one app does not confer authority in another.

**Entitlement**:
An authoritative right to access a paid feature or content. A product can
grant rights in one app or several without merging their catalogs.

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

# Account notification producer adoption

ContentKit accepts `Deps.Notifications` (`helpers/notify/postgres.TxSink`) supplied
by the optional AuthKit account inbox. The host owns construction; ContentKit owns
comment notice meaning. General retained event logs and preference syncing remain
independent. No wildcard IDs, range deletion or generic dependency graph exists.

## Publication and withdrawal

Approved replies use `comment.reply:<reply ID>`; likes preserve their existing
`comment.liked:<reaction revision>` identities. A source-owned
`content_comment_notifications` table retains only exact emitted identities,
recipient, comment association, source revision/state and original time. It does
not contain inbox payloads, read state or delivery cursors. The source metadata is
needed to withdraw historical like-transition IDs that the current reaction row
no longer retains. Explicit migration backfills known legacy identities without
changing their chosen receipt IDs or read/archive state. Known old uppercase reply
identities are adopted to their canonical UUID spelling; an existing canonical
receipt wins collisions, and old aliases become hidden tombstones. Scoped adoption
classifies the known native source to its owning app before revisioned writes.
Only pre-revision legacy rows may acquire classification; already-versioned
unclassified rows require explicit source-state adoption rather than hash rewriting.
The migration also withdraws receipts whose comment or parent was already deleted
or hidden before the upgrade.

Deleting reply B withdraws B's notices. Deleting parent A withdraws notices for A
and the reply-to-A notices for its replies while keeping those replies intact.
Moderation hiding does the same; restoration reopens visible reply notices under
the same exact identity with newer revisions. The producer never lists an inbox.
A recreated/reopened notice becomes unread; editing an active notice preserves
read state. Source mutation plus publish/withdraw obligation shares one transaction.

The publisher either writes into the account owner's inbox in that transaction,
or durably queues remote delivery in the source transaction. An acknowledgment
loss/retry preserves identity. A late lower revision cannot undo a withdrawal.
Terminal account erasure drops subsequent notices through the account owner's
fences; temporary directory/database failures remain errors.

## Host wiring and frontend

Supply the AuthKit scoped publisher as `contentkit.Deps.Notifications` and mount
`engine.RouteGroups()` rather than requesting the legacy notification routes.
AuthKit exposes the account inbox. ContentKit keeps existing required account/app
cleanup and restore fences using the adopted shared scope during transition.

The browser client accepts an injected `NotificationTransport`:

```ts
const content = createContentKitClient({
  baseUrl: "/api/v1/contentkit",
  notifications: authClient.notifications,
});
```

The existing NotificationBell/NotificationInbox/BillingNotification presentation
uses that transport. Account refresh/read/archive changes invalidate the shared
ContentKit notification resources; content HTTP is not used for inbox operations.

## Retiring billing inbox polling

The runtime `contentkit_openrails_inbox_sync` job and `OpenRailsNotifications`
construction method are removed. Existing general `ListEvents` readers remain
available for host behavior. The legacy Source.Consume adapter remains solely for
explicit data handoff. Stop old writers, drain accepted legacy work, apply exact
OpenRails-to-AuthKit receipt adoption, and invoke Source.RetireInboxSync. It verifies
source progress before cancelling only that source's waiting polling jobs, refuses
running jobs, and preserves history/other sources. Never perform this cutover as
an implicit application-start mutation.

The full cross-repository design is tracked in AuthKit #470 and ContentKit #248
in open-rails/tracker. Source implementation is separate from package release and
controlled deployment.

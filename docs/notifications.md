# Account notifications

AuthKit owns the user inbox, its storage, HTTP API, client and UI components.
ContentKit receives the scoped `authkit/notify.Publisher` returned by
`accounts.Notifications("contentkit")` as `contentkit.Deps.Notifications`.
A nil publisher disables user notifications. ContentKit creates no fallback
inbox, mounts no notification routes and exposes no notification client or UI.

The publisher has two operations: `Publish` creates or updates exact producer
identities; `Withdraw` removes obsolete identities from the active inbox.
Both accept bounded batches and an optional existing PostgreSQL transaction.
ContentKit supplies its source transaction, so the comment/reaction change and
accepted inbox update or durable remote delivery either commit together or
roll back together. It never performs a remote network write inside that
transaction. AuthKit binds the tenant, producer provenance and app visibility.

## Comment events

A visible reply publishes `comment.reply:<reply UUID>` to the parent author.
A like publishes one `comment.liked:<comment UUID>:<liker id>` notice per liker
and comment: unliking withdraws it and liking again reopens the same notice.
Self replies/likes and anonymous recipients do not produce notices. Payloads
carry content/comment references and the actor ID, not copied comment bodies.
Hosts resolve those references through their normal content access rules.
Hiding an account withdraws its comment-like notices and suppresses notices for
likes made while hidden. Unhiding publishes its current likes with the same
identities only for approved, undeleted comments whose authors are not hidden.

Deleting or hiding a parent preserves its replies, but withdraws their notices
to that parent. Deleting or hiding a reply withdraws that reply's exact emitted
notice. Restoration publishes a newer revision with the same deterministic ID;
AuthKit reopens it unread. Source retries reuse a revision and stale delivery
cannot revive a newer withdrawal. No wildcard ID ranges or inbox scans occur.
`content_comment_notifications` is ContentKit's source transition ledger; it
contains no inbox payload or user read/archive state.

## Host publication and UI

An application obtains its own producer from AuthKit:

```go
publisher, err := accounts.Notifications("application")
if err != nil { return err }
err = publisher.Publish(ctx, []notify.Publication{{
    RecipientID: accountID,
    ID: "task:" + taskID,
    Revision: revision,
    Kind: "application.task-ready",
    Data: payload,
    CreatedAt: createdAt,
}})
```

Later, the authoritative producer calls `Withdraw` with the same recipient/ID
and its next durable revision. Local read/unread and archive/restore actions
remain independent of that business resolution.

The browser uses AuthKit's `authClient.notifications`, `NotificationInbox` and
`NotificationBell`. OpenRails' BillingUI owns billing notice wording and actions;
ContentKit has no intermediate notification transport or duplicate components.
General retained platform event logs and `preference_sync` are independent.

## Account cleanup

The host's AuthKit inbox scope for ContentKit must be `contentkit:` followed by
ContentKit's account key (`tenant_<tenant ID>` for a scoped engine); account
cleanup reads erasures under exactly that scope.

Account lifecycle cleanup retains its established canonical account/app erasure
fences and content-only restore checks. The narrow cleanup adapter calls
AuthKit's canonical erasure implementation; it creates no inbox tables and
provides no inbox reader or producer surface.

# Notifications

Account inbox ownership belongs to the optional AuthKit notification add-on.
Hosts pass its transactional publisher as `contentkit.Deps.Notifications`.
ContentKit supplies comment notices and presentation; Helpers supplies the shared
notification model and storage. The AuthKit owner verifies recipients and serves
personal inbox/read/archive operations. See [account notification adoption](account-notifications.md).

With an injected publisher, ContentKit does not mount its legacy notification
routes. The local store and HTTP adapter remain compatibility/recovery facilities
for explicit staged adoption and existing account erasure fences. Native content
mutations use the injected publisher exclusively; they do not duplicate receipts.

## Native and host-authored notices

ContentKit automatically publishes:

- `comment.reply`: an approved reply to another registered person's published
  comment, including first publication through editing or moderator approval.
  Replaying the same approval does not duplicate the receipt. Restoring a hidden
  reply reopens its exact identity at a newer producer revision, unread.
- `comment.liked`: another signed-in person's actual transition to a like on a
  published comment. Retrying the same like does not notify twice; a later
  unlike and like creates a new event. Self and anonymous likes do not notify.

The interaction and notification commit in the same transaction. Payloads
contain content/comment/actor references, never copied comment bodies or
anonymous IPs. Resolve references through current content access checks when
displaying or following them; retained history does not grant access.

Hosts publish their own selected kinds through an explicitly configured AuthKit
producer. Its binding fixes the account tenant, source namespace and app visibility:

```go
publisher, err := accounts.Notifications("application")
if err != nil { return err }
err = publisher.ApplyTx(ctx, sourceTx, []notify.Mutation{{
    Input: notify.Input{
        Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
        Origin: notify.Origin{ID: stableIssueID},
        Kind: "export.ready",
        Data: json.RawMessage(`{"export_id":"..."}`),
        CreatedAt: occurredAt,
    },
    Revision: sourceRevision,
}})
```

Commit `sourceTx` only after this succeeds. Local publication joins that database
transaction; a configured remote publisher records its durable delivery obligation
there. `Apply` instead acknowledges durable destination acceptance when there is
no source transaction to join. Resolve an obsolete notice with the same identity,
a higher source revision and `Withdrawn: true`; never list the inbox to discover
producer IDs. Batches contain at most `notify.MaxMutations` entries.

An origin identifies the producer and stable source issue or event. Same-revision
replays are idempotent; conflicting same-revision content is rejected. An older
delivery cannot undo a withdrawal. Newer active updates preserve local read/archive
state; an explicit newer publish after withdrawal reopens the receipt unread.
Use verified account IDs and issuer/directory association, never matching email
addresses. Payloads are user-visible JSON without credentials or operator details.

## Inbox and frontend

AuthKit serves the enabled account inbox through its authenticated personal API.
Use `authClient.notifications` for list/detail, read/unread and dismiss/restore.
Every recipient comes from the verified account and application context; query
parameters cannot select another user or tenant. Only configured trusted producers
can publish. Supply this transport to the existing ContentKit presentation:

```ts
const client = createContentKitClient({
  baseUrl: "/api/v1/contentkit",
  notifications: authClient.notifications,
});
```

For deployments completing an explicit migration, the **legacy ContentKit fallback**
still provides the following routes under its configured API mount. They are absent
when `Deps.Notifications` is injected. Its trusted `PublishNotifications`/
`POST /v1/notifications/publish` and workflow transition operations are compatibility
facilities; new host producers use the revisioned AuthKit publisher above.

| Method and path | Behavior |
| --- | --- |
| `GET /notifications/inbox?limit=20&cursor=...&unread=true` | Active receipts, newest first; at most 100 |
| `GET /notifications/inbox?dismissed=true` | Archived receipts instead of active ones |
| `PUT /notifications/{id}/read` | Mark the local receipt read |
| `DELETE /notifications/{id}/read` | Mark the local receipt unread |
| `PUT /notifications/{id}/dismiss` | Archive the local receipt |
| `DELETE /notifications/{id}/dismiss` | Restore the receipt to the inbox |

Mutations return the canonical `Item` with server timestamps and revision.
Reading or archiving changes only the receipt. A tracked item with
`status: "open"` remains open after either operation. The API exposes no generic
browser action that claims to repair a billing problem or complete source work.
Producers resolve the domain issue and withdraw the exact notice in the same source
transaction. Reading or dismissing it is never evidence that a payment succeeded.

With the injected transport, the browser SDK exposes `client.notifications.list()`, `.setRead()` and
`.setDismissed()`. The React package provides `useNotifications` and
`useNotificationUnread`. Styled `NotificationInbox` and `NotificationBell`
components provide filters, pagination, error/retry states, read/archive/restore
controls, tracked-status labels, and a keyboard-accessible popover. The bell's
dot means unread items exist; it does not invent a total from a partial page.

```tsx
<ContentKitProvider client={client} viewer={currentUser.id}
  accessRevision={currentUser.accessRevision}>
  <NotificationBell
    renderItem={item => renderAppNotification(item)}
    renderActions={item => renderAuthorizedActions(item)}
  />
  <NotificationInbox renderItem={item => renderAppNotification(item)} />
</ContentKitProvider>
```

The required `renderItem` handles arbitrary source kinds, including a fallback
for future ones. Components contain no domain actions. `BillingNotification` supplies the
OpenRails billing templates; hosts provide their authorized action URLs. They use ContentKit's appearance/localization provider and support
English, German, Spanish, Japanese, Korean and Chinese. Keep `viewer` and
`accessRevision` current so cached data is cleared when identity/access changes.
Signed-out viewers do not fetch inboxes. Local mutations synchronize mounted
views; an older in-flight refresh cannot overwrite a successful receipt change.
Refreshing one inbox also refreshes mounted unread indicators, so a resolved
notice cannot leave a stale dot on the bell.

The bell checks unread presence every 30 seconds while the page is visible;
`unreadRefreshInterval={false}` disables that. Inbox pages do not poll by default,
so loading more stays stable. Hosts can call `client.notifications.refresh()`
when their own event channel signals a change.

## OpenRails notifications

OpenRails deliberately publishes selected billing notices through the same
AuthKit-owned channel and withdraws resolved issues by deterministic identity.
`Engine.OpenRailsNotifications` and the `contentkit_openrails_inbox_sync` worker
have been removed. Do not attach a periodic event-to-inbox reader to the host
fleet. General OpenRails events remain available for independent host logic.

`notification/openrails.Source.Consume` is retained for explicit one-time legacy
handoff. With old writers stopped, drain previously accepted pages to the saved
cursor, adopt exact legacy receipt identities into OpenRails's current issue
identities through AuthKit's migration operation, then call
`Source.RetireInboxSync` for the source's namespace and River schema. Retirement
refuses a running old worker or facts still beyond the saved cursor and preserves
all unrelated jobs, terminal history, source events and inbox receipts.

A historical billing fact is not current debt state. Fetch the current billing
state before presenting an action. Reading/dismissing a notice never settles a
payment; the billing owner withdraws the notice when its actual issue resolves.

## AuthKit and application events

AuthKit exposes a tenant-scoped retained log through `ListEvents`. A host can
read that log, select the facts relevant to its product, and publish notices
with stable source IDs. It owns its cursor, retries and notification policy.
There is no AuthKit `Deps.OnEvent` callback or per-event delivery job.

Do not copy a whole audit payload into a user notice. Select safe fields and
verify the recipient against the current tenant directory. User-initiated
actions need no inbox notice unless the application has a specific reason.

AuthKit's required account lifecycle removes and fences its inbox. ContentKit's
required cleanup handles its source identities, interactions and legacy store;
it does not own a separately configured AuthKit inbox. During adoption the shared
physical schema retains existing erasure fences. Neither cleanup depends on an
optional general event reader, and delayed delivery cannot recreate erased receipts.

## Shared account inboxes

Sites sharing an AuthKit account directory can keep separate catalogs and use one
AuthKit inbox. Configure the AuthKit add-on against its account database, declare
tenant/app producer bindings, and inject the relevant publisher into each module.
AuthKit enforces tenant-wide versus app-specific visibility. Shared billing
read/archive state belongs to the same receipt in both sites.

Existing installations can adopt their physical schema and logical scope in place.
The following **legacy ContentKit storage declaration** remains for staged
migration, account cleanup and avatar sharing; it does not transfer inbox API
ownership back to ContentKit when the publisher is injected:

```go
cfg.Accounts = &contentkit.AccountsConfig{
    Schema: "account_notifications",
    Namespace: "accounts",
    // Optional: this engine stores avatars when Primary is empty; secondary
    // engines set Primary and Key to the authenticated avatar primary.
    Avatars: &contentkit.AvatarsConfig{},
}
```

The legacy `Namespace: "accounts"` corresponds to `contentkit:accounts`.
Scoped engines derive their namespace from the verified tenant unless an explicit
migration adopts an older scope. AuthKit's tenant-to-scope configuration must match
the adopted value. Native social origins retain ContentKit's app identity; source
provenance is separate from visibility. Omitting the injected publisher preserves
the old local/fallback behavior for staged migration. The older
`Config.Notifications` spelling remains available for inbox-only compatibility;
do not configure it together with `Config.Accounts`.

Configure the AuthKit account inbox against the existing shared schema and
explicitly map the account tenant to its adopted logical scope. Pass a scoped
publisher to each producer at construction. Both hosts retain their shared
merchant/account identity and app visibility; no normal billing inbox consumer
or additional per-site copy is created.

The default selection is payment failure, actual access expiry/revocation
(`access_ended: true`) and an applied asynchronous downgrade
(`downgrade_applied: true`). Signup, routine renewal, cancellation confirmation
and direct checkout/change failures are not delivered by this policy. The raw
OpenRails producer history remains authoritative and complete.

`BillingNotification` and `isBillingNotification` provide reusable templates in
all bundled languages. Pass only real host destinations via `actions`:
`update_payment_method`, `authenticate_payment`, `resubscribe`, `contact_support`
and the `manage_billing` fallback. Retry guidance is an immutable event snapshot,
including its scheduled timestamp. It does not claim that a historical failure
is still open or that an old retry is still scheduled. Current billing pages
remain authoritative for available recovery actions.

Existing stores can be consolidated explicitly with
`notify/postgres.Store.MergeFrom(ctx, []postgres.LegacyScope{...})`. Stop writers
for the cutover, migrate the destination and merge all old scopes before
starting either host on the shared configuration. The maintenance API leaves
sources intact, deduplicates by recipient and origin, preserves read/archive
state and copies account erasure fences before receipts. It is not a startup
hook and never copies the content catalog.

### Account avatars

`Config.Accounts.Avatars` uses ContentKit's existing avatar primary and forwarding
implementation. Both sites use the same AuthKit subject as the avatar ID and
one shared `accounts/user/<subject>` manifest path. A primary owns the original,
crop, derived renditions and worker journal; a secondary forwards authenticated
editor reads and writes to it. Secondary requests cannot choose another person's
avatar, and account erasure remains the primary's responsibility. Each site's
media host must route the account namespace to the primary's bucket. Catalog
media stays in its site's namespace. This shares the authoritative avatar
without creating a second avatar record, copying media or changing catalog
schemas. Existing `Config.Avatars` is unchanged when `Config.Accounts` is absent.

# Notifications

ContentKit is an application's user-notification aggregator. It accepts native
comment events, OpenRails producer events and notices selected by the host,
stores local receipts, and serves the application's inbox and UI. The reusable
`github.com/open-rails/helpers/notify` package owns persistence, idempotency,
read/archive state, tracked workflow state and erasure. Hosts own identity
mapping and domain action destinations. ContentKit provides shared billing
selection, scheduling and presentation; hosts can customize other event kinds.

`contentkit.New` installs the shared store in ContentKit's schema, scoped to
`contentkit:<tenant>`; the shared library has its own ledger.

## Native and host-authored events

ContentKit automatically publishes:

- `comment.reply`: an approved reply to another registered person's published
  comment, including first publication through editing or moderator approval.
  Reapproval of the same reply does not notify twice.
- `comment.liked`: another signed-in person's actual transition to a like on a
  published comment. Retrying the same like does not notify twice; a later
  unlike and like creates a new event. Self and anonymous likes do not notify.

The interaction and notification commit in the same transaction. Payloads
contain content/comment/actor references, never copied comment bodies or
anonymous IPs. Resolve references through current content access checks when
displaying or following them; retained history does not grant access.

Hosts publish their own selected kinds through the engine's client (`POST
/v1/notifications/publish`, `root:notifications:publish`, in one transaction):

```go
_, err := ck.Client().PublishNotifications(ctx, []notify.Input{{
    Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
    Origin: notify.Origin{System: "host-app", Scope: tenant, ID: stableEventID},
    Kind: "export.ready",
    Data: json.RawMessage(`{"export_id":"..."}`),
    CreatedAt: occurredAt,
}})
```

An origin identifies the producer deployment, namespace and stable source event.
Its optional `Subject` preserves the original recipient when the host maps that
account to a local user. Replaying an origin for the same local recipient keeps
the original payload and local receipt/workflow state. Hosts must authorize
recipient IDs; matching email addresses is not an account association. Data is
a user-visible JSON object without credentials or private operator details.

## Inbox and frontend

The notification route group serves these person-only routes. Every recipient
comes from the authenticated subject; query parameters cannot select another
user, tenant or operator audience. Only the host's service publishes.

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
Trusted hosts perform their domain action and then use shared
`Transition`/`TransitionTx` when they actually own a tracked workflow.

The browser SDK exposes `client.notifications.list()`, `.setRead()` and
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

The bell checks unread presence every 30 seconds while the page is visible;
`unreadRefreshInterval={false}` disables that. Inbox pages do not poll by default,
so loading more stays stable. Hosts can call `client.notifications.refresh()`
when their own event channel signals a change.

## OpenRails producer events

OpenRails records billing facts in its retained event log. ContentKit reads
`GET /v1/app/events` (or the identical embedded `Client.ListEvents` API) and
owns the destination cursor. Reading the source never registers a consumer,
acknowledges a message or deletes an event.

The typed adapter uses the same client for embedded and remote OpenRails:

```go
source, err := billingadapter.NewNotificationSource(merchantClient, "openrails")
if err != nil { return err }
jobs, err := ck.OpenRailsNotifications(source,
    billingadapter.SameAccounts(accounts.UserInfo(), accountIssuer))
if err != nil { return err }
// Include jobs in the host's River fleet before starting it.
```

For HTTP without the OpenRails Go module, construct the same source directly:

```go
source, err := openrails.New(openrails.Options{
    Name: "openrails:billing-production",
    URL: billingURL, // before /v1
    MerchantID: merchantID,
    Token: applicationToken,
})
```

The shared inbox stores one cursor per origin and account namespace. Both sites
therefore continue the same import. Each bounded page commits its selected
notices and continuation in **one database transaction**. The thirty-second
poller keeps one active job per source and drains at most ten pages per run;
a failed run is retried by the next tick rather than delayed per-event jobs. A lookup or write
failure rolls back the whole page. Competing replicas compare the saved cursor
under a row lock, so a stale reader cannot overwrite progress. Replaying an old
page preserves receipt identity and read/archive state.

The default selection covers unexpected payment failure, actual access ending
and applied downgrades, rather than notices for direct user actions. Other
billing facts and optional notices advance the cursor without creating inbox
items. Erased or absent accounts are skipped; directory outages are retried.
The retained customer UUID and issuer establish identity even after billing
rows are deleted. The typed mapper accepts native accounts and explicitly
allowed external issuers; an unknown issuer never proves account ownership.

Source retention is independent of reader progress. An expired or reset cursor
returns an explicit error and requires a deliberate resync; it never silently
drops a missing interval. Historical rows deleted before event-log adoption
cannot be reconstructed by this adapter.

Requests have bounded size and timeouts and refuse redirects. Monetary values
remain JSON strings. An action-requested notice describes the historical fact:
fetch current billing state before offering an action.

## AuthKit and application events

AuthKit exposes a tenant-scoped retained log through `ListEvents`. A host can
read that log, select the facts relevant to its product, and publish notices
with stable source IDs. It owns its cursor, retries and notification policy.
There is no AuthKit `Deps.OnEvent` callback or per-event delivery job.

Do not copy a whole audit payload into a user notice. Select safe fields and
verify the recipient against the current tenant directory. User-initiated
actions need no inbox notice unless the application has a specific reason.

`Engine.EraseSubjects` removes and permanently fences the inbox before other
content cleanup. Delayed imports cannot recreate it. Required account cleanup
remains separate from optional event readers.

## Shared account inboxes

Sites sharing verified account IDs can keep separate catalogs and share only
notification storage. Configure the same notification schema and namespace:

```go
cfg.Accounts = &contentkit.AccountsConfig{
    Schema: "account_notifications",
    Namespace: "accounts",
    // Optional: this engine stores avatars when Primary is empty; secondary
    // engines set Primary and Key to the authenticated avatar primary.
    Avatars: &contentkit.AvatarsConfig{},
}
```

The engine migrates the dedicated schema and writes all account receipts into
`contentkit:accounts`. Native social notices retain `Origin{System:
"contentkit", Scope: cfg.Tenant}`. Every site's authenticated API restricts
reads, unread pages, pagination and receipt mutations to that site's native
origin plus explicitly admitted shared origins. An ID from another site is
not readable or writable through this site's API. Shared billing read/archive
state is the same receipt on both sites. Erasing an account fences all origins
in its shared inbox. Omitting `Config.Accounts` and `Config.Notifications` preserves the existing
per-tenant store and unrestricted trusted producer selection. The older
`Config.Notifications` spelling remains available for inbox-only sharing. Do not
configure it together with `Config.Accounts`.

Once the OpenRails client exists, attach the source before `Start` and compose
the returned River contribution with the host fleet:

```go
source, err := openrailsadapter.NewNotificationSource(billing.Client(), "openrails")
if err != nil { return err }
notificationJobs, err := ck.OpenRailsNotifications(source, openrailsadapter.SameAccounts(auth.UserInfo(), accountIssuer))
if err != nil { return err }
// Include notificationJobs alongside ck.RiverJobs() in the host's River fleet.
```

Both hosts use the same source name and account namespace. The contribution
registers a source-specific shared queue. Each bounded attempt commits inbox
changes and the destination cursor together and safely retries failures.
No host billing-kind filter or poller is needed. `SameAccounts` is suitable only
when the billing customer ID is the very same verified AuthKit subject; use a
verified association mapper otherwise. No email comparison establishes identity.

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

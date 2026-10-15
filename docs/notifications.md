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

OpenRails owns billing facts and a durable producer stream. ContentKit consumes
it through `notification/openrails`, without a dependency on the OpenRails Go
module or a second billing inbox UI.

```go
source, err := openrails.New(openrails.Options{
    Name: "openrails:billing-production", // complete origin system, used verbatim
    URL: billingURL,                      // before /v1
    MerchantID: merchantID,
    Consumer: "contentkit-site-a",        // stable independent destination name
    Token: applicationToken,             // func(context.Context) (string, error)
})
if err != nil { return err }
if err := source.Register(ctx); err != nil { return err }

result, err := source.Consume(ctx, publisher, // a notify.Publisher; consumption moves into the engine (#117 follow-up)
    func(ctx context.Context, event openrails.Event) (string, bool, error) {
        // Resolve a verified merchant/customer association from host records.
        // false deliberately skips this event; an error leaves it pending.
        return lookupLocalRecipient(ctx, event.MerchantID, event.CustomerID)
    }, openrails.ConsumeOptions{Limit: 100})
```

The consumer is independently namespaced by the authenticated application and
merchant. Registration is idempotent and persists across worker restarts. Each
call reads a bounded pending page and ACKs only after destination publication
commits or the host deliberately skips an event. No timestamp or cursor is used
as a delivery watermark. A failed source ACK safely replays publication without
resetting read/archive state. One mapping/publication failure leaves that event
pending while allowing other events in the page to proceed. Host scheduling
retries returned errors; `Retire` is an explicit deprovisioning action, not normal
worker shutdown.

Optional events default to skip plus ACK. Set `IncludeOptional: true` to map
them; the mapper can apply per-user preferences and explicitly skip unwanted
ones. Unmapped accounts are skipped only when the host returns `deliver=false`.
Erased recipients are also a deliberate skip. Lookup/storage failures are not
silently acknowledged. Every page is checked against the configured merchant;
source customer IDs remain in `Origin.Subject`. All event kinds are accepted,
and JSON numbers are never round-tripped through floating point.

The source's `action_requested` flag records a request at event creation; it is
not evidence that a current issue remains open. Imported events remain general
notices. Fetch current OpenRails domain state before offering a billing action.
The adapter uses bounded requests and idempotency keys, refuses redirects, and
never reads operator findings or acknowledges a different consumer's stream.
Historical availability before registration follows the producer's retention
policy; this adapter does not reconstruct deleted pre-subscription history.

## Optional AuthKit/application callbacks

An existing host callback can use the same publisher. For example, AuthKit's
`Deps.OnEvent` receives committed `iam.Event` values with stable `ID`, `UserID`
and `OccurredAt`; its durable delivery retries errors. The following is an
**opt-in host policy**, not a built-in AuthKit notification catalog:

```go
deps.OnEvent = func(ctx context.Context, event iam.Event) error {
    if event.Kind != iam.EventUserEmailChanged { return nil }
    userID, deliver, err := lookupAuthAccount(ctx, accountIssuer, event.UserID)
    if err != nil || !deliver { return err }
    _, err = ck.Client().PublishNotifications(ctx, []notify.Input{{
        Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
        Origin: notify.Origin{System: "authkit:" + accountIssuer,
            Scope: accountIssuer, Subject: event.UserID, ID: event.ID},
        Kind: string(event.Kind), CreatedAt: event.OccurredAt,
        Data: json.RawMessage(`{}`), // select only fields this user should see
    }})
    return err // an erased recipient's notice is dropped, not an error
}
```

Compose this into existing callback work, explicitly ignore unselected/future
kinds and return persistence errors for retry. Do not copy the entire audit event
or manufacture random event IDs on each retry. This example uses AuthKit
v1.24's durable `Deps.OnEvent` contract, alongside the native authentication and
user-info contracts used by ContentKit. Notification policy remains opt-in.

`Engine.EraseSubjects` removes
and permanently fences the user inbox before other content cleanup. Delayed
imports cannot recreate it. Interactions on retained published comments skip
notification to an erased recipient. Keep failed host erasure obligations
pending until the whole erasure succeeds.

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
source, err := openrailsadapter.NewNotificationSource(billing.Client(), "openrails", "shared-account-notifications")
if err != nil { return err }
notificationJobs, err := ck.OpenRailsNotifications(source, openrailsadapter.SameAccounts(auth.UserInfo()))
if err != nil { return err }
// Include notificationJobs alongside ck.RiverJobs() in the host's River fleet.
```

Both hosts use the same source name, consumer and account namespace. The
contribution registers one durable source consumer and a source-specific shared
queue. Each bounded attempt persists before ACK and safely retries failures.
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

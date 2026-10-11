# Notifications

ContentKit uses `github.com/open-rails/helpers/notify` and its PostgreSQL store.
The shared library owns notification identities, pagination, read state,
workflow state, transactional publication and account erasure. ContentKit owns
user authentication, native comment events and its OpenRails source adapter.
There is no additional ContentKit notification table or delivery worker.

`contentkit.Migrate` installs the shared store in the host-selected schema.
`Runtime.Notifications` and `Runtime.Content.Notifications` are the same store,
scoped to `contentkit:<tenant>`. Embedded hosts can publish their own kinds with
`Publish` or join a domain transaction with `PublishTx`:

```go
_, err := rt.Notifications.Publish(ctx, notify.Input{
    Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
    Origin: notify.Origin{System: "host-app", Scope: tenant, ID: stableEventID},
    Kind: "content.available",
    Data: json.RawMessage(`{"content_id":"..."}`),
    CreatedAt: occurredAt,
})
```

An origin is the producer deployment, its namespace and a stable event ID.
Replaying it for the same recipient preserves the original payload and local
read/workflow state. Hosts must authorize recipient IDs; matching email
addresses does not establish an account association. Data must be a user-visible
JSON object, without credentials or private operator details.

Native events are automatic:

- `comment.reply`: an approved reply to another registered person's published
  comment, including first publication through editing or moderator approval.
  Reapproval of the same reply does not notify twice.
- `comment.liked`: another signed-in person's actual transition to a like on a
  published comment. Retrying the same like does not notify twice; a later
  unlike and like creates a new event. Self and anonymous likes do not notify.

The interaction and notification commit in the same transaction. Their payloads
contain content/comment/actor references, never copied comment bodies or
anonymous IPs. A host resolves references through current content access checks
when displaying or following them; retained history does not grant access.
Notification delivery preferences and display choices belong to the host.

The authenticated user API is mounted under `Runtime.Handler()`:

| Method and path | Behavior |
| --- | --- |
| `GET /notifications/inbox?limit=20&cursor=...&unread=true` | Newest-first page (`items`, `next_cursor`), at most 100 |
| `PUT /notifications/{id}/read` | Mark this user's local notice read |
| `DELETE /notifications/{id}/read` | Mark this user's local notice unread |

The caller's verified subject selects the user inbox. Query parameters cannot
select another recipient, tenant or operator audience. Applications cannot use
these person-only routes, and no browser ingestion endpoint exists. The browser
SDK exposes `client.notifications.list()` and `.setRead(id, read)`; hosts choose
labels, links, badges and UI layout.

Read state and workflow state are separate. An item with `status: "open"` still
requires action after it is read. ContentKit exposes no generic browser endpoint
that claims to repair billing by changing notification status. Trusted host code
can use shared `Transition`/`TransitionTx` after authorizing and completing its
own domain action.

## OpenRails integration

`notification/openrails` consumes the merchant-authorized app API without
pulling in the full OpenRails Go module:

```go
source, err := openrails.New(openrails.Options{
    Name: "billing-production", // stable across URL changes
    URL: billingURL,            // before /v1
    MerchantID: merchantID,
    Token: applicationToken,    // func(context.Context) (string, error)
})
// The host obtains this association from its trusted account records.
binding := openrails.Binding{CustomerID: billingCustomerID, RecipientID: userID}
result, err := source.Sync(ctx, rt.Notifications, binding, openrails.ScanOptions{
    Cursor: savedCursor,
    MaxPages: 10,
})
```

The token requires `merchant:customers:read`. Each page is checked against the
configured merchant and mapped customer before any notice is imported. All
customer event kinds, including future strings, are accepted. JSON numbers are
not round-tripped through floating point. Origin retains the stable deployment
name, merchant ID and source notification ID. Source workflow status is retained
as a historical snapshot; source `seen`/`read_at` is ignored.

The host owns scheduling. Continue a bounded scan using `result.NextCursor`;
retry the same cursor after failure (partially imported pages are idempotent).
When `result.Complete` is true, restart periodically from an empty cursor. Do not
turn the latest timestamp into a permanent watermark: late commits can appear
behind it. Source fetches have deadlines and response/page limits and refuse
redirects. This path never acknowledges OpenRails host events or marks source
notifications seen.

This copies the **retained customer inbox**, not a guaranteed historical event
archive. OpenRails may supersede or expire notices before a delayed scan. The
destination retains imported history until its own retention or erasure policy
removes it, so an imported `open` snapshot may be stale after a source resolution.
For current billing action/status, fetch OpenRails domain state before acting.
Operators' findings remain in the operator audience and are never imported into
customer inboxes.

`Runtime.EraseSubjects` (and standalone `content.Runtime.EraseSubjects`) erases
and permanently fences the user's shared inbox before other content cleanup.
Delayed imports cannot recreate it; interactions on retained published comments
skip notification to an erased recipient. The host must keep failed erasure
obligations pending and retry until the full erasure succeeds.

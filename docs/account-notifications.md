# Account notification adoption

ContentKit accepts `Deps.Notifications` as `authkit/notify.Publisher`, created by
AuthKit's client for one configured producer. It exposes exactly `Publish` and
`Withdraw`, each accepting a batch and optionally the source `pgx.Tx`. AuthKit
owns persistence, receiver authorization, local receipt state, queued remote
delivery, the authenticated HTTP API and browser UI. ContentKit owns only the
comment/reaction decisions and durable source IDs/revisions. Nil disables this
channel without changing general platform events or recommendations.

Existing deployments must perform one coordinated stopped-writer upgrade.
Changing Go package ownership does not rename the existing notification tables,
legacy scope, migration versions, advisory locks or receipt IDs.

1. Stop both host fleets and all billing/content writers. Take a fresh private
   recovery point and restore-verify it. Do not run old writers against the new
   producer/adoption cohort.
2. Migrate AuthKit's optional inbox and both ContentKit catalog/source ledgers
   explicitly with the new binaries. ContentKit no longer creates or validates
   an inbox as part of ordinary catalog startup. Its explicit comment adoption
   reads an existing legacy inbox only when one exists; it preserves canonical
   UUID identity, local read/archive state and erasure guards.
3. Finish the already-established billing event cursor through
   `notification/openrails.Source.Consume` using the explicit
   `migration/legacyinbox` adapter. Receipt import and cursor commit remain
   atomic. No source event is ACKed or deleted, and cursor expiry requires
   explicit recovery rather than silently skipping an interval.
4. Export exact billing notification identity/order adoption from OpenRails.
   Persist the new private snapshot and its directory before applying it through
   AuthKit. The host command uses a new exclusive snapshot path, no automatic
   retry with that path, and retains it with the database recovery point.
5. Adopt receipts in bounded batches, preserving chosen IDs, read/archive state,
   canonical source identities and hidden revision fences. Restore source
   ordering alongside any later financial archive restoration.
6. Retire only this legacy source's unfinished
   `contentkit_openrails_inbox_sync` jobs in both host River schemas. Running
   jobs or remaining source events refuse retirement. Other jobs, histories and
   general event readers are preserved.
7. Resume the matching new host and frontend cohort. Verify AuthKit inbox
   list/read/unread/dismiss/restore, cross-user/app denial, unseen withdrawal,
   stale delivery rejection and newer unread reopening. Verify no old polling
   importer or ContentKit notification routes remain.

`content_comment_notifications` keeps only the exact emitted comment/reply/like
identity and producer revision. It never copies notification payloads or user
read state. Deleting a parent preserves the thread and withdraws each exact
reply-to-parent notice; deleting a reply withdraws its own notice. The existing
parent locks, rollback behavior and moderation/restoration rules remain.

`Accounts.Schema`/the adopted account namespace still select shared account and
avatar metadata. They no longer configure a ContentKit-owned inbox. Existing
canonical account/app erasure read guards and source cleanup replay remain
separate from notification publication; they are not an inbox fallback.

# Restore

Search documents and derived counts are rebuildable. Authored content,
preferences and their revision sequence, moderation state, canonical signals
and erasure fences are durable state.

Restore a backup into a store initialized with the matching ContentKit
baseline and host schema. Preserve MigrateKit's PostgreSQL migration ledger
with the databases it tracks. The current initializer is not an upgrade tool
for snapshots taken under older feature-specific migration chains; restore
those with their matching library version before a verified host-owned import
into fresh stores.

After restoring PostgreSQL, rebuild keyword documents from the host's current
content and recompute taxonomy counts when the backup predates catalog
changes. Do not rewind preference revisions against a retained sink: run
`SeedPreferenceRevisionFloor` before writers resume, as described in the
[host integration guide](../HOST_INTEGRATION.md#preference-boundary-reactions-and-favorites-into-the-signal-plane).

After restoring ClickHouse, replay every subject deletion newer than the
backup from the host's deletion ledger, then run `EnforceErasures` for each
tenant. Run `RepairProjections` and `RefreshCoEngagement` as needed to rebuild
derived state from canonical signals. `signal.CheckSchema` must pass before
serving analytics.

Erasures never roll back. Replay post-backup deletions and permanent source
fences before reopening content writes; an AuthKit callback acknowledgement
means durable acceptance, not completion of downstream erasure.

## Media

The media bucket is versioned with a 30-day noncurrent expiry
(`s3.Store.Configure(ctx, 30)`), so sweeps and folder deletions stay
recoverable for 30 days. To restore to time T:

1. Restore PostgreSQL to T (the steps above).
2. Run `store.Restore(ctx, "{tenant}/", T)`: manifests, public slots and slot
   originals return to their versions at T (those created after T are
   removed), and blobs and originals the restored manifests reference lose
   their delete markers. `RestoreReport.Missing` lists references whose
   versions have expired.
3. Re-apply media erasures made after T (`EraseUserTx`/`DeleteItemsTx`) from
   the host's deletion ledger.

The RGW→R2 mirror must keep deleted objects at the destination for at least
30 days to cover the same window. Erased files therefore remain as backup data
(RGW versions and the R2 mirror) for 30 days; the privacy policy must say so.

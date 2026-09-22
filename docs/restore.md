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
changes. Do not rewind preference delivery revisions against a retained sink:
run `SeedPreferenceRevisionFloor` before writers resume, as described in the
[host integration guide](../HOST_INTEGRATION.md#preference-boundary-reactions-and-favorites-into-the-signal-plane).

After restoring ClickHouse, replay every subject deletion newer than the
backup from the host's deletion ledger, then run `EnforceErasures` for each
tenant. Run `RepairProjections` and `RefreshCoEngagement` as needed to rebuild
derived state from canonical signals. `signal.CheckSchema` must pass before
serving analytics.

Erasures never roll back. Replay post-backup deletions and permanent source
fences before reopening private writes; an AuthKit callback acknowledgement
means durable acceptance, not completion of downstream erasure.

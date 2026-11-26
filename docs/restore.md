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
content. Entity counts and automatic covers derive from current facets on
reads. Do not rewind preference revisions against a retained sink: send
`POST /v1/import` with `preference_floor` before writers resume, as described in the
[host integration guide](../HOST_INTEGRATION.md#preference-boundary-reactions-and-favorites-into-the-signal-plane).

After restoring ClickHouse, replay every subject deletion newer than the backup from the host's deletion ledger (`Engine.ReplayErasureFences`), then run `signal.Store.EnforceErasures` for each tenant. Call `Engine.ResyncPreferences` so the preference job re-sends every reaction and favorite. Run `RepairProjections` as needed to rebuild derived state from canonical signals. `signal.CheckSchema` must pass before serving analytics.

Erasures never roll back. Replay post-backup deletions and permanent source
fences before reopening content writes; an AuthKit callback acknowledgement
means durable acceptance, not completion of downstream erasure.

## Media

The bucket is the only record of media, and it never has versioning (README "Media"): recovery inside it is the manifests' grace. A file an edit stops referencing stays in its item's folder for 7 days (a release's leftovers an hour; withdrawn, taken-down and erased files are due at once) before GC deletes it.

After restoring PostgreSQL, run `media-worker rebuild`: it refills `content_media_items` from the bucket, which wakes releases and GC. Then re-apply media erasures made after the backup (`Engine` erasure, `Jobs.Erase`) from the host's deletion ledger.

Bucket backups are a mirror's job. Restoring one is copying its objects into a new bucket that never had versioning and running `media-worker rebuild`. Erased files remain in a mirror for as long as it keeps deleted objects; the privacy policy must say so.

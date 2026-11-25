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

The media bucket is versioned. Noncurrent versions expire after 7 days, and
a day after a version is replaced only the 3 newest noncurrent versions of
each object stay (the Doujins and Hentai0 buckets get these rules from
doujins-gitops `scripts/media_rgw_policy.py`; `s3.Store.Configure(ctx, days)`
sets only the age limit). Retired permanent files and folder deletions stay
recoverable for 7 days. Hash-named files are written once, so the version
limit does not touch their data; it limits objects rewritten in place: the
manifest, public stable names and the erasure fence (a restore needs only the
manifest). A manifest that changed more than three times after T cannot return
to T from a day after the first of those changes, and a single upload already
writes it several times. Delete markers count among the three, and ContentKit's
cleanup deletes its whole key list again on every retry, so a key it retries
can keep its data only a day. Temporary uploads and regeneratable caches have
a separate seven-day expiration and one-day noncurrent retention; they are not
durable originals. To restore to time T:

1. Restore PostgreSQL to T (the steps above).
2. Restore each content item: `s3.Store.Snapshot(ctx, item, T)` reads the
   item's manifest and the exact object versions it referenced at T and changes
   nothing; `mediastore.Jobs.Restore` then publishes that snapshot through the
   upload journal, copying each file from its old version to a fresh key under a
   fresh incarnation. Public presets are regenerated, not restored, and items
   created after T are not removed. If the manifest version at T or any version
   it references has expired, `Snapshot` fails for that item with
   `ErrNotFound`, the same error as for an item that did not exist at T. No host
   route or command calls these yet.
3. Re-apply media erasures made after T (`Erase`/`RetireItemsTx`) from
   the host's deletion ledger.

An RGW→R2 mirror must keep deleted objects at the destination for at least
7 days to cover the same window; it copies current objects only, so it cannot
bring back an older manifest version. Erased files therefore remain as backup
data for 7 days in RGW versions, and for as long as a mirror keeps deleted
objects (its script's default is 30 days); the privacy policy must say so.

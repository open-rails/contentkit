# Content retirement

A deleted version keeps its media, unserved, during the configured
restoration period, which defaults to 30 days. Deleting the last version also
soft-deletes its parent. Once retention expires,
`contentkit_content_purge_deleted` owns both physical media erasure and
removal of the corresponding catalog records.

The purge first claims each version's erasure (`purge_claimed_at`) in a short
transaction under the same parent/version locks used by restoration, and
commits it before any storage call; from then on restoring that version is
refused. Outside any lock it then writes the item's permanent fence and an
empty manifest whose files are all retired and due, and deletes them. An unclaimed
sibling or a newly created version can still revive an automatically deleted
parent. Parent restoration checks only the versions it actually exposes.

Storage failure leaves the deleted records for the same purge operation to
retry. Successful cleanup removes only the captured version IDs after a final
eligibility check. Each captured parent/version is attempted independently:
failed rows remain for retry while successful unrelated deletions commit, and
errors are returned after that bounded progress. Each scan advances through a
bounded content-ID or version-ID keyset, including captured failures and locked
candidates. It never restarts at the same failed prefix within the run; the
existing time/context budget still bounds work. A recently deleted parent does
not postpone an older version's own purge deadline. Sibling media and newly
eligible items are not substituted into the captured batch. The small empty manifest tombstone and permanent fence remain
to reject late writers and old access tokens; source files and renditions are
erased.

A worker's late write into an erased folder is an orphan: the item's
garbage collection (`contentkit_media_gc`) deletes it, since an erased item's
manifest names nothing. There is no separate version-media reaper or
per-version folder-deletion job.

Entity, poll and account media retirement tombstone and erase the same way. A
deleted entity's media is fenced from its deletion; retirement makes that fence
permanent before deleting the files. Account avatar erasure is synchronous.

The same sweep erases the purged content's ClickHouse engagement and history
records. Its exact cleanup references are committed with the PostgreSQL purge
and survive removal of the content rows. A ClickHouse outage leaves those
references pending without holding back unrelated PostgreSQL or media deletion.
After ClickHouse acknowledges the deletion, the sweep removes the references;
a lost acknowledgment is safe to retry. There is no separate
`contentkit_forget_contents` worker or timer.

Purge never mutates OpenRails products, prices, purchases or payment history.
The host manages product archival directly through OpenRails; those billing
records remain intact after the content's media disappears.

Older binaries do not check the purge claim when restoring a version, so
they must not run alongside this purge implementation.

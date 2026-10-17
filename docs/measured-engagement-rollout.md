# Measured engagement rollout

The foundation adds `/engagement` and separate ClickHouse segment, state,
daily and co-engagement tables. Opening a reader is not a measured event.
Snapshots contain loaded visible page ranges, allocated attention, actually
played timeline ranges and a resume selector. A receipt and revision identify
one cumulative segment; repeated delivery cannot add attention or coverage.

`Config.MeasuredEngagement` defaults to `false`. Keep it disabled while any
application or account-erasure executor still runs the old storage protocol.
Apply the additive migration through the deployment's explicit migration
command. It leaves existing writers' tables intact and copies useful legacy
progress/resume into `legacy_reading_state`, without copying counter fields.
No ingestion handler applies migrations.

Upgrade both applications and every erasure executor before activation. Run
erasure enforcement across all catalog keys, verify completion, then enable
measured writes. The shared erasure ledger fences late writes; new erasure
passes also remove measured segments, projections and both co-engagement
lineages. A legacy executor alone cannot certify physical deletion of tables
it does not know.

The remaining integration replaces legacy consumption counters throughout
queries, ranking, SDK components and host instrumentation. After the final old
writer has stopped, retirement must refresh the legacy progress copy and
remove obsolete counter projections, source consumption rows and imported
counter columns. The feature is not complete until that post-cohort cleanup
has run. Legacy progress can remain visible as history; it must never become
measured quality or popularity evidence.

Validation runs on GitHub Actions. `generate-contract` accepts an exact source
ref and publishes generated API files with `contract-head.txt`; apply only the
artifact matching the intended source commit, then run the normal CI suites.

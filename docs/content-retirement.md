# Content retirement

Implementation target for the content-purge ownership change; the current
release still hands storage deletion to a separate media job.

A deleted version keeps its media during the configured restoration period,
which defaults to 30 days. Deleting the last version automatically deletes
its parent. Once retention expires, the existing
`contentkit_content_purge_deleted` operation owns both physical media erasure
and removal of the corresponding catalog records.

Storage failure leaves the deleted record available for the same purge
operation to retry. Physical cleanup is idempotent and preserves other
versions' media. Database removal follows verified storage deletion; the
purge operation also owns any recorded repeat needed for an upload already
in flight. It does not hand that responsibility to another River job or a
separate media reaper.

Restoration and purge must serialize before either takes catalog row locks.
No database transaction is held while object storage is being erased.
Expired versions cannot be restored into media that purge has already
started retiring.

Billing products remain archived and purchase records remain intact after
the physical content disappears.

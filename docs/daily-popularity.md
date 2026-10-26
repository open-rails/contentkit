# Daily participation popularity

The ranking worker prepares one scalar row per available content item, UTC day
and configured domain/language scope. A person contributes at most once to an
item that day. At least one edition must have one second of measured full-access
attention and nonempty measured coverage, after canonical receipt revisions
replace retries. Several editions or repeat visits do not create another daily
participant. A return on a different day contributes again.

For each day, `U` is its qualified participants and `N` is its active eligible
population. The score is `U / (N + 20)`. A bucket averages those scores over the
item's available days, including available days with zero readers. It is daily
participation, not the number of distinct people over an entire year. The former
engagement/completion/approval/revisit weights are not part of this score.

Availability comes from recorded publication/access history, never today's rule
applied to old days. A day counts once if the item was available for any part of
it, including a short launch day. An edition public at any time makes the day's
audience public; otherwise the available tiers are unioned. A person observed
with any matching eligible tier at any time that UTC day belongs to the day's
eligible audience. Public days include every observed active person. This
deliberate whole-day model does not reconstruct noon access transitions. Unknown
group/PPV audience sizes are not invented. Instrumentation coverage still bounds
the history that can be prepared.

The existing worker calls `popularity.Prepare` once per kind/scope for its due
bucket windows, then `Rank` reads only the daily scalars. Overlapping buckets
share the prepared days. Today and yesterday remain provisional because activity
receipts are valid for 24 hours. A day gets one final calculation after its end
plus that reporting window; a worker restart cannot leave an earlier provisional
calculation permanently frozen. Finalized days are reused indefinitely.

`popularity.Rebuild` or `signal.Store.PrepareDailyPopularity(..., true)` explicitly
recalculates historical corrections/imports. There is no additional job or
automatic repair loop. Raw engagement and population observations remain durable
sources. Preparation errors reach the worker; the previous stored ranking stays
served, while a later refresh retries provisional daily work. Derived day writes
are not a cross-table transaction and can be partial until that retry.

Daily rows contain anonymous counts, not subjects. Account erasure continues to
fence and remove canonical/per-subject state; historical anonymous counts may
remain. Explicit rebuild honors those fences, and normal provisional refresh
excludes erased subjects. Aggregate retention is independent of raw erasure.

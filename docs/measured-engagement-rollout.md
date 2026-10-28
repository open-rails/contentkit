# Measured engagement

Measured engagement uses `/engagement` and separate ClickHouse segment, state,
daily tables. Opening a reader is not a measured event.
Snapshots contain loaded visible page ranges, allocated attention, actually
played timeline ranges and a resume selector. A receipt and revision identify
one cumulative segment; repeated delivery cannot add attention or coverage.

The shared erasure ledger fences late writes; erasure also removes measured
segments and projections. Imported legacy progress can remain visible as
history; it must never become measured quality or popularity evidence.

Validation runs on GitHub Actions. `generate-contract` accepts an exact source
ref and publishes generated API files with `contract-head.txt`; apply only the
artifact matching the intended source commit, then run the normal CI suites.

## Retired surfaces and meaning

There is no public raw open count, `counts.views`, `content.views`,
`most_viewed` sort, imported `view_baseline`, browser `.view()` session, or
`useViewSignal` hook. `/signals` accepts explicit clicks; reading uses
`/engagement`. Opening a route, preloading, or repeating a snapshot cannot
qualify a person as a reader.

Window metrics count qualified distinct people, authenticated/guest readers,
completers, allocated active milliseconds, and each person's bounded
coverage/attention quality. One person contributes at most one quality unit
per work in the window. Returning readers require measured attention on two
UTC days. Measurement source rows retain historical free/premium access;
normalization uses a new population lineage with stable browser identities,
not old tab sessions. Legacy resume/progress is excluded from recommendation
seeds, measured account completions, and ranking evidence.

A gallery page needs at least one second of accumulated allocated attention
before its covered cells count as read; short positive exposures accumulate
across segments/visits. Overall person qualification requires one second of
measured attention and actual coverage. Pages idle after 60 seconds without
input; a focused, visible, actually playing video can continue without pointer
input. Pauses, seeks, hidden tabs and unloaded/preloaded elements contribute
nothing. Gaps longer than five seconds between browser observations are
unobserved, so the tracker drops them rather than inventing attention.

## Importing existing bookmarks

`importer.Batch.LegacyReading` (`legacy_reading`) accepts
`LegacyReadingRecord{Target, UserID/AnonKey, FirstReadAt?, LastReadAt, Progress,
ProgressMax, Completed, Resume, Revision}`. `Target.Content` restores work
progress; `Target.Version` also retains that edition's progress. Version
bookmarks get a work resume copy so they remain discoverable in account
history. Exactly one subject identity is required. Last-read time orders
updates; revision orders updates at the same instant; duplicate delivery is
idempotent. Erasure fences apply during every insert.

These facts never become loaded-page observations, elapsed attention,
qualified people, measured completions or popularity evidence. A bookmark at
page10 does not claim that pages1 through9 were read. Importers should set
`Completed` only when preserving an existing source fact, not infer it from
the last-position bookmark. Bare opens and legacy all-time counts have no
replacement import route.

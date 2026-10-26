# Popularity policy

Public popularity measures daily qualified participation. One account or
anonymous browser identity contributes at most once per content item per UTC
day. Returning on another day contributes again. Repeated opens, page turns,
translations and visits on the same day do not create additional people.

This is a ranking of sustained daily interest, not a count of distinct people
across an entire period. Raw engagement remains available for search evaluation,
recommendation work and explicit recalculation.

## Daily score

For each available item and UTC day:

```text
U = qualified people who participated that day
N = observed active people eligible for that day's audience
participation = U / (N + 20)

bucket_score = mean(participation over the item's available days in the bucket)
```

The additional 20 eligible nonparticipants are a reliability prior. They keep a
single reader in a tiny audience from dominating the ranking. Available days
with no readers contribute zero. Days before publication or outside trusted
history are not invented. Every included day has equal weight, including a
partial launch day. Only items with measured participation receive a score.

The score has no per-period unique-reader count, page-quality multiplier,
completion multiplier, explicit-feedback multiplier or returning-reader bonus.
Measured progress, completion, explicit reactions and favorites retain their own
product uses; popularity does not reconstruct them to build a date range.

## Qualified participation

The reader reports focused active time and actual loaded, visible page coverage;
the player reports actual playback coverage. A person qualifies after at least
1,000 active milliseconds and nonempty measured coverage in an edition during
the UTC day. Positive short segments of that edition can accumulate toward the
threshold. Qualifying in more than one edition still contributes one person to
the work's daily total.

The canonical winning revision of each receipt supplies its contribution.
Retries and older cumulative snapshots do not multiply attention. Full-access
measurements are required; preview-only engagement and legacy route-open counts
do not become qualified participation. Opening a route or preloading an image
alone is not a reading event.

## Eligible audience

Eligibility uses historical availability and the day's observed active people,
within the configured application, content kind, domain and language scope.
Registered-account totals and today's subscriber totals are not historical
active populations. A premium user active on free content still belongs to the
premium-eligible audience.

The calculation deliberately uses whole UTC days:

- If any edition is public during the day, the work has a public daily audience.
- Otherwise the known tiers that could open an edition during the day are
  unioned. A person with a matching known tier observation at any time that day
  is eligible. Multiple tiers and observations still count the person once.
- Unsupported group or purchase-only eligibility has no invented denominator;
  a known public or tier edition must establish the work's audience.

This trades exact intraday timing for reusable daily results. For example, a
person who acquires a subscription in the evening can belong to that day's
premium audience even if an edition was premium only in the morning. The raw
receipts still retain their actual access context; access enforcement does not
use this approximate analytics rule.

## Preparation and retention

The existing `contentkit_popular_refresh` worker prepares daily history for the
host's configured kinds/scopes and builds declared presets without waiting for
public traffic. Each day's raw engagement is reduced once per scope, shared by
all preset and arbitrary day ranges.

Current and recently ended days remain provisional through the existing
24-hour activity-receipt lifetime. Once that reporting window closes, a final
successful preparation is retained indefinitely. Ordinary refreshes reuse
finalized days. Historical imports, corrections or a changed scoring definition
require an explicit `popularity.Rebuild`; no automatic historical repair sweep
is added.

Daily rows contain anonymous item-level counts, not user identities. Account
erasure still removes/fences canonical and per-subject data. Historical aggregate
counts can remain; an explicit rebuild respects the erased source. This keeps
routine analytics approximate without coupling account erasure to rebuilding
every historical ranking.

A preparation failure preserves the previous stored ranking. Raw engagement
remains authoritative, and the next scheduled attempt can refresh provisional
results. Empty days receive a preparation checkpoint so they do not trigger
repeated raw scans merely because they have no rows.

## Host-configured buckets

Each content kind declares an ordered list of `kinds.PopularityBucket` entries:

- A stable ID and optional display label.
- Exactly one window: rolling `Days`, fixed UTC `From`/`Until`, or `All`.
- Explicit domain/language scopes; omitted scopes mean one global scope.
- A refresh interval, defaulting to one hour.

Fixed dates are inclusive/exclusive: November 1 through November 23 uses
`From: "2024-11-01"` and `Until: "2024-11-24"`. Rolling windows include the
current UTC day. All time means the retained trusted history, not invented
engagement before instrumentation.

The first applicable configured bucket is the default. Public
`sort=popular:<bucket-id>` selects a declared stored ranking. An arbitrary whole-day
range uses `sort=popular&activity_from=YYYY-MM-DD&activity_until=YYYY-MM-DD` and
combines prepared daily rows. Neither path creates custom cache entries or
queues raw computation. Shared preset choices come from the host's content
model. Publication age remains independent:
`published_within` filters releases without changing the selected activity bucket.
Staff statistics and `top_rated` retain their separate time-window semantics.

The earliest supported date is the first complete UTC day with trustworthy
catalog and population history. If instrumentation began at noon, this is the
following midnight. Requests before that cutoff or beyond prepared coverage
receive an explicit error; missing history is not silently clipped or counted
as zero. Coverage includes successful empty days. Today may be included once
prepared and remains provisional through its reporting window.

Preset pages read stored ordered IDs and apply normal catalog visibility and
filters. Arbitrary-range pages compute ordered IDs from daily scalars and apply
the same catalog query. Before the first preset result, the page is empty and its popularity
snapshot is not ready. A prepared empty result is ready. Snapshot metadata
identifies the stored result and its dates rather than claiming a previous
UTC day's snapshot is current. Stored rankings remain readable during a
ClickHouse outage.

## Performance evidence

The [daily participation experiment](../benchmarks/daily-participation/README.md)
combines 36.5 million synthetic item/day rows into 100,000 sorted UUID item scores
for a 365-day period in 1.37–1.40 seconds of ClickHouse time on the recorded
machine, using two query threads. These are warm/cache-assisted local results
for combining already prepared counts, not a production latency guarantee or
an end-to-end raw-engagement benchmark.

Daily source preparation is measured separately. A 60,000-user day with ten
distinct items per person means 600,000 user/item-days, before multiple receipts
and revisions. Reducing that day's raw data is reusable work; combining a date
range does not revisit those receipts. More configured scopes increase
preparation and storage work. Raw data, finalized daily counts and prepared
rankings have distinct costs and must be reported separately.

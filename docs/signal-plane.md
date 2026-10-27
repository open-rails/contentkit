# Signal plane

The signal plane separates measured consumption from explicit feedback and
retained legacy navigation. A route opening, thumbnail preload, or latest page
number is not evidence that someone read any preceding pages.

Subjects are tenant/app-catalog scoped account IDs or stable browser identities.
Guest history stays in local storage across tabs; account history is authoritative
on the server. Resolving a stored guest content ID always applies current domain,
access and content-filter policy before returning its card.

## Sources and projections

- `engagement_segments` stores bounded cumulative visit/segment snapshots with
  immutable access-receipt times. Each revision replaces its predecessor. Images
  contribute only their actually loaded, visible ranges and allocated active
  attention; players contribute actual played ranges, not seeks or elapsed route
  time. Exact retries never increment anything.
- `engagement_state` projects each subject's union of covered units, per-page
  attention, active time and resume selector for a version and its work. A work
  selects its best edition's coverage rather than concatenating translated editions.
- `engagement_daily` is a repairable per-subject/version/day projection. Repair
  computes one complete source generation before bounded partition writes.
- `legacy_reading_state` preserves old bookmarks and progress without treating
  them as measured engagement. It remains useful after raw counter retirement.
- `signals` stores explicit clicks and current reactions/favorites. Superseded
  feedback revisions replace old values; clicks alone cannot create readership.
- `CoEngaged` computes co-engagement from the canonical selected window; no
  pairs cache is stored.
- `exposures` records served/rendered/visible cards and original rank positions.
  It is attribution data, not a count of readers.
- `measured_population_observations` and `measured_population_coverage` describe
  trusted event-time eligibility for free/premium audience normalization. Retired
  per-tab identities are never copied into this denominator.
- `erasures` permanently fences erased identities. New writes, reads, projection
  repair and operator retirement all honor the fence.

## Measurements and transport

Use `useReadingEngagement` in a stable content-page parent. Attach `reading.page`
to actual images and pass `engagement={reading}` to `VideoPlayer` or
`VideoMiniPlayer`. Page changes remain local. The tracker snapshots every two
minutes and on hide/exit, retaining exact revisions for retries. Loaded prefetches,
posters, hover previews, hidden/unfocused tabs and inactive page attention do not
qualify. Real video playback can continue beyond the page idle timeout; seeks,
stalls and unobserved timer gaps do not become watched time.

`POST /engagement` accepts those snapshots with their server-issued activity
receipt. `POST /signals` accepts explicit clicks, not progress or raw opens.
Measured engagement is accepted when its canonical snapshots are saved.
ContentKit then attempts the existing state and daily summaries inline. If that
derived step fails, it logs the failure and still accepts the saved engagement.
Canonical validation and insert failures remain errors; batches can retain rows
accepted before a later failure. Exact receipt/revision retries remain idempotent.

Resume position, progress/completion counts and daily activity can stay stale
until another successful interaction for the same user/item recomputes them.
An untouched item may remain stale indefinitely. There is no automatic repair
job or reconnect-triggered rebuild. Operators can call `RepairProjections`
explicitly for a known incident or calculation change, following `Next` with
`After` until complete. That operation reports its errors. Subject erasure
and content forgetting retain their existing failure reporting.

Positive short measurements accumulate; qualification requires at least one second
of active attention. Pages need one second of attention before their covered units
count toward reading completion. Quality is bounded by actual coverage and eight
seconds per logical page; completion requires 90% measured coverage. Reopening the
same content does not create another person.

## Reads and rollout

`Metrics` returns distinct readers, account/guest readers, measured completers,
active time, bounded quality and explicit feedback. `History` and `States` also
retain legacy resume state; `CompletedCount` and popularity exclude legacy-only
bookmarks. Windows are whole UTC days `[From, To)` and canonicalize revisions before
filtering time, so a corrected receipt cannot count in both its old and new day.

Popularity uses best-per-person quality, returning days and explicit feedback;
see [popularity-policy.md](popularity-policy.md). Audience-normalized ranking
requires trusted access and population coverage. Without it, the ranker explicitly
returns unavailable, allowing discovery's current catalog feedback fallback.

See [measured-engagement-rollout.md](measured-engagement-rollout.md) for what
counts as measured reading.

Old queued `contentkit_projection_repair` rows follow River's ordinary unknown-kind
attempt handling; they do not block startup. See [removed periodic jobs](../HOST_INTEGRATION.md#removed-periodic-jobs).

# Popularity policy

ContentKit ranks measured participation and bounded quality, rather than opens
or page turns. `signal.ContentMetrics` exposes the inputs;
`popularity.Policy` supplies a named, versioned formula. A changed weight or
prior needs a new policy name so caches cannot silently mix formulas.
`Policy.Algorithm` identifies the scoring family independently of its name.

Public discovery defaults to `PolicyV2`, which normalizes participation by the
eligible audience. Explicit `PolicyV1` uses distinct qualified reader reach.
The lower-level `popularity.ByName("")` helper retains `v1`; this differs from
discovery's default. Unknown policy names fail startup.

## Measured evidence

The reader reports loaded, actually visible page ranges and focused active time.
The player reports ranges actually played while visible and focused. Opening a
route, preloading an image, seeking across a movie or advancing its selector is
not evidence that the content was consumed. The normal UI checkpoints cumulative
measurements every 120 seconds and on hide/exit; changing pages stays local.
Stable receipts and revisions replace the same segment on retry.

A subject is an account identity or an anonymous browser identity. It qualifies
as a reader after at least **1,000 active milliseconds** and nonempty measured
coverage in an edition inside the selected window. Positive shorter partials
are retained and can accumulate across segments and visits of that edition.
The API verifies the content/version and its actual length; client-supplied
page totals or premium flags do not establish them.

For page content, the current quality calculation is:

```text
page_quality = min(visible_fraction_of_page, active_ms_on_page / 8000)
edition_quality = sum(page_quality) / actual_page_count
```

Visible geometry is merged rather than counted repeatedly. A single initial
viewport is only a fraction of a tall page. Attention on multiple visible pages
shares elapsed time. Repeated attention can improve the same page's quality up
to one; it cannot add another independent person. The eight-second reference is
the current implementation's quality scale, not a claim that every page takes
exactly that long to read.

For playback, quality is the union of actually played timeline units divided by
the actual duration, bounded to one. Seeking does not fill the intervening gap.
For completion, at least 90% of an edition must be covered. Page coverage only
qualifies as read after that page has accumulated one second of attention;
playback uses actually played units. Completion also requires a qualified reader.

Work-level readers count an identity once across editions. Work-level quality
uses that identity's best edition quality, not the sum or average of visits.
Completing any edition supplies at most one work-level completion. A returning
reader has measured activity on more than one distinct UTC day in the selected
window; changing editions or reopening repeatedly on one day does not suffice.

Retained legacy resume/progress data is separate. It can restore the reader UI,
but never becomes measured quality, popularity reach or account completion
statistics. Imported raw totals and retired route-open events are not ranking
inputs or displayed counters. There is no session-score or raw-view metric.

## Metrics and distinctness

`Metrics` reads canonical measured segments and explicit feedback in a literal
window. Its work or version references are tenant-scoped.

| Metric | Meaning |
| --- | --- |
| `Readers` | Distinct qualified identities across the selected window |
| `UserReaders` / `GuestReaders` | Account/browser identities among those readers |
| `Completers` | Qualified identities completing an edition, once per reference |
| `ActiveMS` | Measured active milliseconds, including positive short partials |
| `ReaderQualitySum` | Sum of bounded per-person quality, at most `Readers` |
| `ReturningReaders` | Qualified identities with activity on multiple UTC days |
| `ValueSum` | Sum of canonical explicit feedback values |
| `PositiveSubjects` / `NegativeSubjects` | Identities whose canonical feedback sums positive/negative in the window |

Feedback remains separate from consumption. Feedback alone cannot give a work
reader reach. A changed preference replaces its previous event; its last-change
UTC day determines window inclusion. Unvotes disappear; retries and reordered
revisions do not multiply a vote.

Staff `GET /stats/activity` returns people distinct **within each UTC day**, plus
signed-in people and active milliseconds. Daily people cannot be summed into
unique people for the period. `GET /stats/top?metric=readers` instead ranks by
period-wide distinct qualified identities; `metric=comments` ranks published
comments. These staff metrics differ from a public popularity score. See also
[the SDK statistics contract](../sdk/ui/docs/statistics.md).

## Eligible-population ranking (`PolicyV2`)

For every immutable publication/access interval, `U` is the distinct full
qualified readers, and `N` is the observed active population eligible to open it.
Both use the same app, content kind, domain, language and time interval.
A premium reader using public content remains eligible for premium content too.
Registered-account counts and today's subscriber count are not substitutes for
historical active eligible populations.

Each interval computes its own quality terms:

```text
raw_participation = U / N
participation = U / (N + 20)
engage  = (quality_sum + 0.40*10) / (U + 10)
finish  = (completers + 0.30*10) / (U + 10)
approve = (positive + 0.75*5) / (positive + negative + 5)
revisit = returning_readers / (U + 10)
interval_score = participation *
                 (1 + 0.35*engage + 0.25*finish + 0.40*approve + 0.10*revisit)
score = sum(available_duration * interval_score) / sum(available_duration)
```

Here approval uses explicit feedback from qualified full readers in that
interval. The 20 additional eligible nonparticipants are a reliability prior,
not a confidence interval or a minimum audience requirement. One reader among
one eligible person has adjusted participation `1/21`; ten among 100 have
`10/120`. Raw participation remains available separately.

There is no fixed premium multiplier. A gallery's paid interval is normalized
against its smaller eligible audience; its public interval against the larger
public audience. Each interval's full score is weighted by availability duration,
not population size. Pooling all numerators first would reintroduce traffic bias.
The initial normalized policy supports public and configured-tier access, not
inferred PPV or group eligibility. A public edition makes the work publicly
available; tier-only editions use their union of eligible tiers.

A signed activity receipt fixes the observed subject, edition, server time and
access/eligibility snapshot. Each new measurement segment obtains a current
receipt; retries retain their original one. If a gallery changes from premium
to public, earlier paid attention keeps its cohort. Preview-only access never
becomes full readership.

Normalization clips every candidate to one common trustworthy coverage window.
When trusted population/availability history is unavailable, the ranker returns
no scored hits, with `ranking.policy=none` and `ranking.mode=standing_unavailable`.
It never silently substitutes unnormalized reader reach for a requested normalized
ranking. The API also states the requested and covered windows. Discovery then
serves its separate standing order (`standing_wilson`/`standing`): one vote per
person from current reactions and favorites changed in the window. It is not a
measured popularity score and cannot recover votes later removed. Explicitly
selecting `v1` remains supported and reports mode `viewer_reach`.
With trusted lineage, discovery's popular lists report mode `measured_then_standing`:
works with measured readers rank first by the normalized score; works with
explicit standing in the window but no measured readers follow by the standing
order. Recent instrumentation or sparse reading therefore never empties a list
that has real votes, and per-work participation is reported only for measured works.
A broken population query is an error, not permission to invent a denominator or
silently reclassify receipts.

## Distinct-reader ranking (`PolicyV1`)

```text
reach   = log10(1 + readers)
engage  = (reader_quality_sum + 0.40*10) / (readers + 10)
finish  = (completers + 0.30*10) / (readers + 10)
approve = (positive + 0.75*5) / (positive + negative + 5)
revisit = returning_readers / (readers + 10)
rank    = reach * (1 + 0.35*engage + 0.25*finish + 0.40*approve + 0.10*revisit)
```

Every quality term is bounded to one, so the multiplier is between 1 and 2.1.
One repeat reader can contribute at most one quality unit and one returning-reader
observation. For 100 readers, that person's maximum effect on the multiplier is
`0.35/110 + 0.10/110`, regardless of how often they reopen the content. The priors
also prevent a lone vote from dominating a small audience.

`Policy.RankExpr()` renders this formula in ClickHouse for global ranking;
`Policy.Score` applies it in Go to candidate metrics. Neither rank is a public
counter. The low-level `signal.Store.Popular` API also supports a separate
`RankWeights` default when no named `RankExpr` is supplied; hosts should use the
named popularity ranker for the policies described here.

## Minimum evidence and time windows

One qualified identity is enough for a positive rank. There is no default
five-reader, vote-count or completion quorum. A positive short partial below the
qualification threshold contributes active time but does not yet supply reader
reach. A normalized interval also needs trustworthy nonzero eligible population;
an impossible reader count exceeding that population is reported as an error.
The ranker returns no scored hits for an empty activity window.

Named windows are literal whole UTC days: `7d`, `30d`, `90d`, `365d`, `all`.
The finite windows include today and move at UTC midnight. Time selects activity;
it does not decay observations within the window or filter publication age.
The shared public grid offers 7, 90, 365 days and all time, so old galleries can
trend again when people engage with them now.

An explicit historical window uses inclusive/exclusive UTC dates:

```http
GET /contents?kind=gallery&sort=popular&activity_from=2026-04-01&activity_until=2026-05-01
```

Both dates and the kind are required. Use literal `sort=popular`, not a
named-period suffix. The SDK equivalent is
`client.discovery.list(popularBetween("2026-04-01", "2026-05-01", {kind: "gallery"}))`.
Reader/domain filters apply before pagination; cursors bind the selected window.

Publication age is independent: add `published_within=7d` to explicitly restrict
to recent releases, for example `sort=popular:7d&published_within=7d`. Cursors
retain the first page's age cutoff, but do not freeze later score or catalog edits.

## Entity ranking, bounds and verification

`Ranker.Entities` derives entity popularity from its member works; artists, tags,
series and characters do not receive independent consumption measurements.
It sums scores for the top `EntityCandidateLimit` works (default 2,000), deduping
each membership. Its `Readers` is a sum of member reader counts, **not unique
people across the entity**. `ContentCount` and `MeanScore` describe only that
bounded slice. `Ranker` also defaults `MaxDepth` to 2,000 and cache TTL to five
minutes; the policy, window, scope and bounds belong to the cache identity.

The native fixtures cover measured size-independent quality, feedback, time
windows, edition overlap, returning days, deduplication/reordering, tenant
isolation, entity aggregation and Go/SQL parity. The viewer-influence fixture
starts 100 people with one second on a single page (quality sum 12.5). Giving one
person repeated attention saturates only that person's quality, producing 13.375;
giving all 100 a second one-second observation yields 25 and 100 returning people
when it occurs on a later day. Exact active time stays separate from quality.
These are acceptance cases, not a claim that a receipt or an unexecuted query
proves production readiness.

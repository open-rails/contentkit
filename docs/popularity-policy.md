# Popularity policy

ContentKit exposes raw named metrics (`signal.ContentMetrics`, `Metrics`,
`Popular`) and a host-selected ranking policy (`popularity.Policy`). A policy is
a named version with fixed, bounded weights and priors. Hosts configure only the
name: `popularity.ByName(name)` at startup (an unknown name is an error,
never a silent default) and every cache key carries it. Public discovery defaults
to `v2`; an explicitly selected `v1` is still respected. The generic
`popularity.ByName("")` helper retains `v1` for existing library callers.
Changing a weight or prior is a new name, never an edit of an existing one.
`Policy.Algorithm` identifies the scoring family independently of that version
name. Copying `PolicyV2` to a newly named, tuned policy therefore preserves
eligible-population normalization.

## Current discovery ranking (`PolicyV2`)

V2 replaces raw viewer reach with participation in the eligible audience. It
retains approval, completion, return visits and reading engagement relative to
the selected version's length. Public and premium content therefore compete
on audience participation and quality rather than absolute traffic.

Each gallery's immutable publication/access history defines availability
segments. For each segment, `U` is its distinct full readers and `N` is the
observed active population eligible to open it, in the same interval, app,
content kind, domain and language. Paid readers browsing free content still
contribute to the paid eligible population. Registered-account totals and
current subscription counts are not historical audience measurements.

```
raw_participation = U / N
reach = U / (N + 20)
segment_score = reach * (1 + .35*engage + .25*finish + .40*approve + .10*revisit)
score = sum(available_duration * segment_score) / sum(available_duration)
```

The extra 20 eligible non-readers are a modest reliability heuristic, not a
confidence interval. Ten readers among 100 eligible subjects produce 8.33%
adjusted participation; 10,000 among 100,000 produce 9.998%. One of one produces
4.76%, rather than 100%. Raw participation is exposed separately. Quality is
also calculated within each segment before averaging its full score; pooling
raw viewers across segments would bring back the large-audience bias.

An access change splits a gallery's history. Earlier paid observations retain
their original audience even after the gallery becomes free. Weights use
availability duration, never audience size. Editions share one work-level
population and duration; a public edition makes the work publicly accessible,
while tier-only editions use their distinct union of eligible tiers. The
initial normalization supports public and configured-tier access, not inferred
PPV or group-specific eligibility.

The browser requests bounded batches from `POST /activity-contexts` when a
view or display actually begins. A signed receipt binds the subject, target,
version, server timestamp and access snapshot. Revisions reuse that receipt;
client-provided premium flags cannot set the cohort. Preview consumption is
excluded from full readership. Host media keys sign a separate activity
purpose; non-media hosts can provide an `ActivitySigner`. Missing signing
configuration is explicit legacy mode, not an ephemeral generated key.

Before instrumentation there is no trustworthy historical access classification.
V2 uses a common measured interval for every candidate. If the requested window
has no trusted coverage at all, the entire query explicitly falls back to v1;
it never mixes old raw scores and normalized scores item by item. The response's
`ranking` names the effective policy, mode, requested interval and covered
interval. Each normalized card also carries score, raw participation and
adjusted participation. Displayed view and reaction counts remain raw counts.

## Inputs (per work, per window)

`Metrics` over `subject_content_daily`; every "subjects" metric counts a subject
once.

| metric | meaning |
| --- | --- |
| `Viewers` | distinct subjects with a view session (work level: a subject reading two versions counts once) |
| `Views` | view sessions (checkpoints, retries and reordered deliveries of one event id are one session) |
| `Completers` | subjects with a completed session (`SessionScorer`: >= 90% of the selected version covered) |
| `ScoreSum` | summed session engagement scores (raw public metric) |
| `ViewerEngagementSum` | sum of each viewer's mean session score normalized to [0, 1], clamped per viewer |
| `ReturningViewers` | subjects with more than one canonical view session inside the window |
| `PositiveSubjects` / `NegativeSubjects` | subjects whose current feedback sums > 0 / < 0 |

Windows are the literal `7d|30d|90d|365d|all` whole-UTC-day windows
(`popularity.Periods`, `WindowForPeriod`). Time only decides inclusion: an
observation contributes the same wherever it falls inside the window, and
nothing decays. Work consumption counts once across versions; version coverage
(`Metrics` on `ref.WithVersion(v)`), active consumption and explicit feedback
are separate metrics.

An imported legacy view count (`view_baseline`, [import](import.md)) is no
input: it has no time, viewer or engagement. It only adds to displayed views
and the all-time `most_viewed` sort.

Publication age is a separate filter from the score window. Discovery accepts
`published_within=7d|30d|90d|365d|all`: only currently live, listed, undeleted
publications qualify, based on the same publication timestamp used by newest
ordering and the release feed. A bounded period uses the matching whole-UTC-day
window; `all` has no age cutoff. Pair `sort=popular:7d&published_within=7d` for
this week's popular releases. Cursors retain the first page's cutoff so a
midnight boundary cannot move older or newly published works into that scroll.
This pins publication eligibility, not a snapshot of scores or later edits.

The shared `PopularContentGrid` and `popularQuery` helper use the selected
activity window without adding a publication-age cutoff. Their four tabs mean
activity in the past 7, 90 or 365 days, or all time. Old galleries can therefore
trend again. A host can explicitly supply `published_within` when it wants a
separate list of popular recent releases.

Historical activity windows use the same endpoint and ranking policy:

```http
GET /contents?kind=gallery&sort=popular&activity_from=2026-04-01&activity_until=2026-05-01
```

Both UTC dates are required: the start is inclusive and the end is exclusive.
Use literal `sort=popular`, not a named-period suffix. The kind is required.
Reader visibility and filters still apply before pagination; a cursor is bound
to its kind and date interval. An empty activity window returns no results,
rather than falling back to all-time activity. The SDK equivalent is
`client.discovery.list(popularBetween("2026-04-01", "2026-05-01", {kind: "gallery"}))`.

**Feedback** is each subject's current preference: one replaceable event per
subject × work × axis (`EventID` fixed, `Revision` = mutation time), counted on
the UTC day it last changed. An unvote drops out, like→dislike counts once as
negative, a duplicate delivery is the same event, and a vote cast before the
window counts only in windows that include that day.

## Session engagement (`popularity.SessionScorer`)

```
coverage = min(1, covered / total)                 -- of the selected version
dwell    = min(1, active_s / (covered × SecondsPerUnit))
score    = round(100 × coverage × (0.6 + 0.4 × dwell))      ∈ [0, 100]
```

`covered` is the distinct units exposed (`Progress`, or `Payload[CoveredKey]`
when the host keeps `Progress` as its resume anchor), `total` is
`ProgressMax` of the selected version, so size never enters: a 6-page gallery
read fully scores 100, a 200-page gallery read fully scores 100, the same 200
pages skimmed for 30 scores 15. `SecondsPerUnit` is the host's per-unit dwell
constant (8 s per gallery page; 0 when the units are seconds).

## Ranking (`PolicyV1`)

```
reach   = log10(1 + viewers)
viewer_engagement_sum = sum_over_viewers(clamp(subject_score_sum/(100·subject_views), 0, 1))
engage  = (viewer_engagement_sum + 0.40·10) / (viewers + 10)
finish  = (completers + 0.30·10) / (viewers + 10)
approve = (positive + 0.75·5) / (positive + negative + 5)
revisit = returning_viewers / (viewers + 10)
rank    = reach × (1 + 0.35·engage + 0.25·finish + 0.40·approve + 0.10·revisit)
```

Each term lies in [0, 1], so quality multiplies reach by at most 2.1 and reach
(distinct subjects) dominates. Engagement first averages sessions within each viewer, then gives each
viewer one observation. Fifty re-reads can change that viewer's mean, but
contribute at most one unit to the engagement sum and one returning-viewer
count. For 100 viewers, one repeater can change quality by at most
`0.35/110 + 0.10/110`, independent of their session count. Priors are
pseudo-observations: with `ka = 5` one vote moves `approve` by at most 1/6 of
its 0.40 weight (about 1% of a typical rank).

The same formula runs twice: `Policy.RankExpr()` renders it as a ClickHouse
expression over the metric columns for the global top-N (`Ranker.Popular`);
`Policy.Score` applies it in Go to `Metrics` rows for host-selected candidate
sets (`Ranker.Candidates`, `Scores`: an artist's works, a tag, a search page,
the personalised blend). The fixture asserts both agree on every work within
1e-9 and exactly on the invariance pairs. Public counts (`Hit.Viewers`,
`Hit.MeanEngagement()`) are the raw metrics, never derived from the rank.

## Group popularity (`Ranker.Groups`, `Catalog` port)

Artists, series, tags, characters and seasons never receive signals.
`Groups(kind, groupKind, window)` takes the top `GroupCandidateLimit`
(default 2,000) ranked works, asks the `Catalog` (`*groups.Store`) which groups
each work is in, and sums member scores per group (a work in one group twice
counts once). The listing is approximate: a record whose
members all fall below the cutoff is absent, a record with many lower-ranked
members can rank below one with fewer higher-ranked members, and
`ContentCount`/`Viewers`/`MeanScore` describe only that slice. The bound is part
of the cache key; changing it is a ranking-policy decision.

## Judged fixture (`TestIntegrationPolicyFixture`, real ClickHouse + Keeper)

Works recorded through the real hub into two databases (v1 session scorer,
legacy scorer), 30-day window: no votes; one like; one dislike; 10/10 mixed; 8
likes; 8 dislikes; 25 unvoted viewers; 40 viewers with 30 dislikes; 120 viewers
with 60 dislikes; 10 viewers with 10 likes; one reader × 50 sessions; 5 readers;
10 readers reading twice; 10 readers once; 6-page work read fully; 200-page
work skimmed (30 pages); 200-page work read fully; a work with en (10 pages) and
es (12 pages) versions read by overlapping subjects; the same checkpoints
delivered four times out of order plus likes delivered twice vs. once;
identical sessions at the first hour of the window, today, and the hour before
the window; vote changes (unvote, pre-window like, pre-window like → dislike,
dislike → like, duplicate like). 548 sessions, 155 votes (doujins #893's PR text
counts 528/146 from before its vote-change work was added; the ported fixture is
#893's final one).

| judgment | A baseline | B default rank, v1 scorer | **C v1** | D multiplicative approval | E approval 0.6, no revisit | F flat priors (k=1) |
| --- | --- | --- | --- | --- | --- | --- |
| one like helps a little | fail | fail | pass | pass | pass | pass |
| one like cannot beat five more viewers | pass | pass | pass | pass | pass | pass |
| one dislike hurts a little | fail | fail | pass | pass | pass | pass |
| one dislike hurts less than eight | fail | fail | pass | pass | pass | pass |
| liked > mixed > disliked | fail | fail | pass | pass | pass | pass |
| one vote moves the score under 5% | pass | pass | pass | fail | pass | fail |
| 75% of viewers disliking outweighs double the reach | fail | fail | pass | pass | pass | pass |
| approval bounded: 120 viewers half disliked beat 10 viewers all liking | pass | pass | pass | fail | pass | pass |
| five readers beat one reader's fifty sessions | pass | pass | pass | pass | pass | pass |
| re-reads by the same readers help, under 15% | fail | fail | pass | pass | fail | pass |
| short work read fully beats long work skimmed | fail | pass | pass | pass | pass | pass |
| page count itself is irrelevant | fail | pass | pass | pass | pass | pass |
| duplicate delivery changes nothing | pass | pass | pass | pass | pass | pass |
| shifting observations inside the window changes nothing | pass | pass | pass | pass | pass | pass |
| observations before the window do not rank | pass | pass | pass | pass | pass | pass |
| **total** | 7/15 | 9/15 | **15/15** | 13/15 | 14/15 | 14/15 |

A = ContentKit's default rank (`log10(1+viewers) × max(1, score_sum/(views+10))`)
over the legacy scorer (`-30 + pages×5 + seconds/8`): the corrected baseline. It
ignores votes, lets long works win on raw page counts and counts sessions as
independent evidence. B shows the scorer fix alone fixes size but nothing else.
D lets dislikes erase reach and a single vote moves the rank >5%; E cannot see
re-reads; F's flat priors let one vote swing >5%. C is the only variant
satisfying every original judgment. This prelaunch correction intentionally
changes the old doujins #893 policy: its pooled session mean let one repeat
viewer influence every viewer's engagement, and its session surplus could
saturate the revisit bonus. Host score parity is not an acceptance gate.
`v1` scores, descending:
crowd_disliked 2.7036, short_full = long_full 2.3425, more_viewers 2.2094, liked
2.1418, big_disliked 2.1125, one_like 2.0825, no_votes 2.0605, vote_changes
2.0439, one_dislike 1.9943, mixed 1.9547, two_versions 1.9472, long_skimmed
1.8599, return_readers 1.8302, disliked 1.8164, once_readers = window_start =
window_end 1.7782, ten_likes 1.6810, duplicate = single 1.6578, five_readers
1.2788, repeat_reader 0.4693.

Also asserted: the two-version work has 13 viewers / 18 sessions with per-version
counts and coverage against the selected version (es half-readers score 50, not
60); duplicate deliveries leave metrics byte-identical; vote changes give +2/−1 in
the window and +3/−1 all time; `7d` ranks only today's readers, `30d` everything
but before_window, `90d`/`365d`/`all` everything with before_window = window_start
= window_end = once_readers exactly; a second tenant reusing an id in the same
database is invisible and ranks alone; group scores are the member sums
through the `Catalog` and the signal plane holds no group rows; another policy
name never reads v1's cache entry.

## Adversarial viewer-volume qualification

`TestIntegrationViewerWeightedQuality` records 100 zero-score viewers per
work, then compares one viewer reaching 1,000 sessions against all 100 viewers
returning once. The heavy viewer contributes `0.999` engagement units and one
returning-viewer count, while the audience contributes 50 engagement units and
100 returning-viewers. The test bounds the heavy viewer's score change, requires
the audience-wide behavior to rank higher, and checks raw Views/ScoreSum stay
1099/99900 and 200/10000 respectively. It also verifies negative legacy scores,
feedback-only subjects, literal window exclusion, identical early/late scores,
and ClickHouse/Go ranking parity against the real store. These metrics are
computed at read time from existing per-subject daily numerators; no migration
or re-ingestion is required.

## Host adoption

Hosts rank nothing themselves: `GET /v1/contents?sort=popular:{period}` reads
the scores the engine's `contentkit_popularity` job copies into
`content_facets.popularity`, under every filter, and `POST /v1/signals`
scores views with `SessionScore`. Delete host popularity readers, scorers and
period parsing.

## Operations and limits

- `approve`'s prior 0.75 and the 8 s/page dwell constant are assumptions until
  live data exists; re-derive them as `v2`.
- Sessions recorded under a legacy scorer carry scores in [−30, 100]; `engage`
  clamps each viewer's mean to [0, 1].
- `Recommend → PopularityFor` inside the hub still uses the default rank;
  recommendation-model work is separate.
- Rollups keep numerators and denominators per subject per day, never a stored
  rank, so a later policy re-ranks history without re-ingestion.

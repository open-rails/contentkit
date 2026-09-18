# Popularity policy

ContentKit exposes raw named metrics (`signal.ContentMetrics`, `Metrics`,
`Popular`) and a host-selected ranking policy (`popularity.Policy`). A policy is
a named version with fixed, bounded weights and priors. Hosts configure only the
name: `popularity.ByName(name)` at startup (default `v1`; an unknown name is an
error, never a silent default) and every cache key carries it. Changing a weight
or prior is a new name, never an edit of an existing one.

## Inputs (per work, per window)

`Metrics` over `subject_content_daily`; every "subjects" metric counts a subject
once.

| metric | meaning |
| --- | --- |
| `Viewers` | distinct subjects with a view session (work level: a subject reading two versions counts once) |
| `Views` | view sessions (checkpoints, retries and reordered deliveries of one event id are one session) |
| `Completers` | subjects with a completed session (`SessionScorer`: >= 90% of the selected version covered) |
| `ScoreSum` | summed session engagement scores |
| `PositiveSubjects` / `NegativeSubjects` | subjects whose current feedback sums > 0 / < 0 |

Windows are the literal `7d|30d|90d|365d|all` whole-UTC-day windows
(`popularity.Periods`, `WindowForPeriod`). Time only decides inclusion: an
observation contributes the same wherever it falls inside the window, and
nothing decays. Work consumption counts once across versions; version coverage
(`Metrics` on `ref.WithVersion(v)`), active consumption and explicit feedback
are separate metrics.

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
engage  = (score_sum/(100·views) · viewers + 0.40·10) / (viewers + 10)
finish  = (completers + 0.30·10) / (viewers + 10)
approve = (positive + 0.75·5) / (positive + negative + 5)
revisit = min(1, (views − viewers) / (viewers + 10))
rank    = reach × (1 + 0.35·engage + 0.25·finish + 0.40·approve + 0.10·revisit)
```

Each term lies in [0, 1], so quality multiplies reach by at most 2.1 and reach
(distinct subjects) dominates. Sessions set the engagement *mean*; viewers set
its *confidence*, so one subject's fifty re-reads are one observation. Priors are
pseudo-observations: with `ka = 5` one vote moves `approve` by at most 1/6 of
its 0.40 weight (about 1% of a typical rank).

The same formula runs twice: `Policy.RankExpr()` renders it as a ClickHouse
expression over the metric columns for the global top-N (`Ranker.Popular`);
`Policy.Score` applies it in Go to `Metrics` rows for host-selected candidate
sets (`Ranker.Candidates`, `Scores`: an artist's works, a tag, a search page,
the personalised blend). The fixture asserts both agree on every work within
1e-9 and exactly on the invariance pairs. Public counts (`Hit.Viewers`,
`Hit.MeanEngagement()`) are the raw metrics, never derived from the rank.

## Taxonomy popularity (`Ranker.Taxonomy`, `Catalog` port)

Artists, series, tags, creators, characters and seasons never receive signals.
`Taxonomy(kind, taxonomyKind, window)` takes the top `TaxonomyCandidateLimit`
(default 2,000) ranked works, asks the host `Catalog` which taxonomy records
each work is assigned to, and sums member scores per record (a work assigned
twice to one record counts once). The listing is approximate: a record whose
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
satisfying every judgment. `v1` scores, descending, identical to doujins #893:
crowd_disliked 2.7036, short_full = long_full 2.3425, more_viewers 2.2094, liked
2.1418, big_disliked 2.1125, one_like 2.0825, no_votes 2.0605, vote_changes
2.0439, one_dislike 1.9943, mixed 1.9547, two_versions 1.9544, long_skimmed
1.8599, return_readers 1.8302, disliked 1.8164, once_readers = window_start =
window_end 1.7782, ten_likes 1.6810, duplicate = single 1.6578, five_readers
1.2788, repeat_reader 0.4967.

Also asserted: the two-version work has 13 viewers / 18 sessions with per-version
counts and coverage against the selected version (es half-readers score 50, not
60); duplicate deliveries leave metrics byte-identical; vote changes give +2/−1 in
the window and +3/−1 all time; `7d` ranks only today's readers, `30d` everything
but before_window, `90d`/`365d`/`all` everything with before_window = window_start
= window_end = once_readers exactly; a second tenant reusing an id in the same
database is invisible and ranks alone; taxonomy scores are the member sums
through the `Catalog` and the signal plane holds no taxonomy rows; another policy
name never reads v1's cache entry.

## Host adoption

### doujins (#893)

- Delete `internal/services/discovery/popularity.go` (`PopularityPolicy`,
  `PolicyV1`, `PolicyByName`, `PolicyNames`, `RankExpr`) and `GalleryScorer`;
  register `popularity.SessionScorer{SecondsPerUnit: 8, CoveredKey:
  "unique_pages_viewed"}` for kind `gallery` (versions are
  `ref.WithVersion(id)` of the same kind, so one scorer covers both).
- `popularity.policy` config → `popularity.ByName(name)` at startup, then
  `popularity.New(popularity.Config{Source: hub, Policy: policy, Cache: cache,
  Catalog: catalog})`. `Reader.Policy`/`ParentRankingGalleryLimit` become
  `Config.Policy`/`Config.TaxonomyCandidateLimit`.
- `Reader.popularRanking` → `ranker.Popular(ctx, "gallery", window,
  offset+limit)` sliced; `GalleryPopularity`/`scoreCandidates` →
  `ranker.Candidates`/`Scores`; `PopularEntities`/`parentRanking` →
  `ranker.Taxonomy(ctx, "gallery", "artist"|"series"|"character"|"tag",
  window)` with a `Catalog` over `gallery_artists ∪ galleries.publisher_id`,
  `gallery_series`, `gallery_characters`, `gallery_tags ∪ gallery_version_tags`
  (the `parentGalleryQueries` SQL, keyed by gallery id).
- `WindowForPeriod`, `PopularityPeriods`, `DefaultPopularityPeriod` →
  `popularity.WindowForPeriod(period, time.Now())`, `popularity.Periods`,
  `popularity.DefaultPeriod`.
- `view_count` = `Hit.Viewers`, `avg_engagement` = `Hit.MeanEngagement()`.
- Keep the host fixture as a recorder/API test; the judged fixture lives here.
  The host `docs/popularity-policy.md` keeps only host facts (endpoints, config
  key) and links here.

### hentai0

- `discovery.Reader.PopularVideosPage` calls `Hub.Popular` under the default
  rank and `PersonalizeVideoRanking` calls `Hub.PopularityFor`; replace with
  `ranker.Popular` / `ranker.Scores` so videos rank under the same named
  policy.
- `windowFor` accepts `1d` and silently defaults unknown periods to 30 days;
  replace it with `popularity.WindowForPeriod` (literal 7/30/90/365/all, error
  otherwise) and drop `1d` from the API.
- `VideoScorer` scores `-30 + 130·watched/duration` on the legacy scale;
  replace it with `popularity.SessionScorer{SecondsPerUnit: 0}` (`Progress` =
  watched seconds, `ProgressMax` = duration: score = 100 × coverage, completed
  at 90%). Sessions already recorded keep their legacy scores; `engage` clamps.
- Add `popularity.policy` to config (`v1`) and refuse startup on unknown names.

## Operations and limits

- `approve`'s prior 0.75 and the 8 s/page dwell constant are assumptions until
  live data exists; re-derive them as `v2`.
- Sessions recorded under a legacy scorer carry scores in [−30, 100]; `engage`
  clamps the mean to [0, 1].
- `Recommend → PopularityFor` inside the hub still uses the default rank;
  recommendation-model work is separate.
- Rollups keep numerators and denominators per subject per day, never a stored
  rank, so a later policy re-ranks history without re-ingestion.

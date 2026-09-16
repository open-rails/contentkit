# Signal plane — engagement, history & unseen

The signal plane is the genuinely new part of the system (see [DESIGN.md](DESIGN.md) for the whole
picture). It records **what each subject did and thought about each entity**, and projects that into
fast reads for history, unseen, engagement, and (later) personalized search + recommendations.

## Core idea

```
(tenant, subject, entity_type, entity_id) → signals
```

- **subject** — a resolved user id (from authkit) or, for anonymous traffic, a session key hash
  (`hash(ip|user_agent)` or a cookie id). History/unseen are meaningful only for logged-in subjects;
  anonymous signals still feed popularity/quality aggregates.
- **signals** — both *implicit* (view-session: duration, progress, completion) and *explicit*
  (reactions, ratings — "what they thought").

## Tables

- **`events`** — canonical source events. Identity `(tenant, entity_type, entity_id, subject, signal_type,
  event_id)`; `ReplacingMergeTree(version)` with `version = revision << 64 | content hash`. Every read
  and projection selects the highest version per identity explicitly (`argMax`), so correctness never
  depends on merge timing, batch boundaries or partitions. Retained without TTL as the rebuild source.
- **`subject_state`** — one row per `(tenant, subject, entity_type, entity_id)`: first/last signal,
  canonical event count, views, completions, active seconds, max progress, resume, last view score,
  net feedback. Indefinite compact history.
- **`subject_daily`** — one row per `(tenant, entity_type, entity_id, subject, day)`: events, views,
  completions, active seconds, score and value sums, per-type counts. Windows sum days per subject
  first, so unique viewers over 7/30/90/365/all days are exact and deleting a subject removes exactly
  its contributions.

Both projections are rebuilt from canonical events for every touched key and versioned by the newest
raw ingest time of that key, so a projection that saw more events always replaces one that saw fewer.
`RepairProjections` rebuilds stale or missing projections in bounded, resumable steps.

## The Scorer (per-entity extension point)

```go
type Scorer interface {
    // Map a raw session payload to a normalized engagement score, generic progress,
    // and whether this counts as "completed".
    Score(ctx context.Context, s Session) (score uint8, progress, progressMax uint32, completed bool)
}
```

Examples:

- **gallery** — `progress = max_page_reached`, `progressMax = page_count`,
  `completed = progress ≥ ceil(0.9 * progressMax)`; score from pages + dwell.
- **blog post** — `progress = max_scroll_pct`, `progressMax = 100`,
  `completed = scroll ≥ 90 OR read_time ≥ est_read_time`; score from read-time + scroll.
- **video** — `progress = watched_seconds`, `progressMax = duration_s`,
  `completed = watched ≥ 0.9 * duration`; score from watch %.

The kit owns the storage/upsert/read machinery; the host owns only the `Scorer`.

## Exposures & attribution (evaluation data)

`exposures` records **what was shown**, one row per result list and stage (`served` / `rendered` /
`visible`), identity `(tenant, render_id, stage)` with revisions like events: `query_id`, `surface`,
`ranker`, `language`, `subject`, parallel `entity_types` / `entity_ids` / `positions` arrays,
`occurred_at`. Clicks are ordinary signals carrying `render_id` / `surface` / `position` in their
payload (`Signal.WithAttribution`). `Store.Attribution` joins canonical clicks to the requested
stage's canonical list, yielding `(render, shown placements, clicks with Exposed flag)` and the
clicks whose render has no exposure at that stage.

## Facets & filtering (host-defined)

`unseen` / search / recs accept a host-supplied filter over **generic facets** the hub stores but
assigns no meaning to — set-membership `flags` (e.g. `premium`, `r18`, region), key/value `attrs`,
numeric `num_attrs` — plus the two universal visibility primitives `visible_from` and `deleted`.
Gating like "premium" or "members-only" maps onto facets; the hub never learns what they mean
(mechanism vs meaning). Facets are mirrored alongside the catalog so the unseen anti-join can apply
them without calling back to the host.

## The four reads

### History — "what I've seen"
`subject_state` for the subject, ordered by `last_signal_at DESC`. Filter by `completed` /
`in_progress` (via `max_progress` vs `progress_max`) for "finished" vs "started".

### Unseen — "new stuff I haven't seen"
`entity catalog (for type; visible_from <= now, not deleted, matching the host's facet filter) MINUS
{ entity_id : subject_state row exists for subject with max_progress > 0 }`. Ordered by
`visible_from DESC`. This is the anti-join described below.

### Metrics — named statistics per entity and window
Viewers, views, completions, completers, active seconds, score sum, events, per-type counts and
positive/negative feedback subjects, each defined separately from canonical `subject_daily` rows.

### Popularity — "what's popular in a window"
Ranked entities with at least one view in a window, from `subject_daily`.

- **Windows** are whole UTC calendar days `[From, To)`: `LastDays(n, now)` is the n most recent days
  including today; `Between` takes arbitrary day slices (e.g. `2025-06-05 → 2025-08-08`). Sub-day
  windows are rejected. All events inside a window count equally.

`Popular(entity_type, window, filter, limit)` ranks by a default formula (log-scaled volume × avg
engagement with a Bayesian prior, no time decay); the host can tune the weights or supply its own
ranking expression over the metric columns (mechanism vs meaning). The same projection feeds recs
cold-start and search ranking.

### Annotations & resume — mark up lists with seen-state
For any list already on screen (search results, popular, similar, history), one bulk call
`States(subject, [entity_ids])` returns each entity's standing for that subject: seen?, `% =
max_progress / progress_max` (the YouTube-style progress bar), completed?, and `resume` (where to pick
up). It's a point/`IN` lookup on `subject_state` (keyed by `(tenant, subject, entity_type,
entity_id)`), so annotating a page of cards is cheap. This is how seen-state is *displayed*; `Unseen`
is how it's *filtered out*.

### Similar & recommendations (two shapes)
Both read the same matrix; they differ by anchor:

- **Similar — "more like this" (item → items):** for a given entity, fuse content similarity
  (semantic `SimilarTo` + lexical/shared-tags) with **co-engagement** ("subjects who engaged with X
  also engaged with Y") from the signal matrix. Needs no logged-in user (works on a gallery page for
  anyone); optionally personalized by blending the viewer's affinity. RRF-fuse + MMR for diversity;
  drop the anchor entity (and optionally already-seen).
- **For you — (user → items):** collaborative filtering over the matrix + content from the subject's
  high-signal history; exclude seen; cold-start → popularity. See progress.json.

Personalized search is the same affinity/popularity signals fused into search ranking — a per-request
toggle the host flips (e.g. only for logged-in users), optionally demoting already-seen via
`subject_state`.

## The unseen anti-join (store-split decision)

Unseen needs the **entity catalog** (content plane, Postgres) and the **seen-set** (signal plane,
ClickHouse) together. Options:

- **(a) Current-state in Postgres**, beside the registry → a clean single-store anti-join. Simplest
  at moderate volume; event stream can still live in ClickHouse.
- **(b) Catalog mirrored into ClickHouse** (the current doujins gallery approach) → a pure-ClickHouse
  anti-join, keeping high-volume reads in one analytical store.

Recommendation: events + aggregates in ClickHouse; resolve the anti-join via a synced catalog
(option b) unless a tenant's volume is low enough that option (a) is simpler. Decide per the tenancy
isolation choice in [DESIGN.md §7](DESIGN.md).

## What this replaces

In doujins, this generalizes the existing gallery analytics (`gallery_view_events`,
`user_history_current`, `gallery_interactions`) and **retires the blog "unique visitor" counter**
(`blog_post_visitors` + UV rollups). The counter was never the goal; durable per-user seen-state +
engagement is.

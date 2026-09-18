# Signal plane

The signal plane records what each subject did and thought about each
content reference and projects it into fast reads: history, unseen, view
context, metrics, popularity, co-engagement and attribution.

```
(tenant, subject, content_kind, content_id, content_version_id) → signals
```

- **subject** — a resolved user id or, for anonymous traffic, a session-key
  hash. History and unseen are meaningful only for users; anonymous signals
  still feed aggregates.
- **content reference** — the work (`content_version_id = ''`) or one of its
  versions. Hosts record a work view and, separately, the selected version's
  view with the same event id.

## Tables

- **`signals`** — canonical source events. Identity `(tenant, content ref,
  subject, signal_type, event_id)`; `ReplacingMergeTree(version)` with
  `version = revision << 64 | content hash`. Every read selects the highest
  version per identity (`argMax`), so correctness never depends on merge
  timing. Retained without TTL as the rebuild source.
- **`subject_content_state`** — one row per subject × reference: first/last
  signal, canonical event count, views, completions, active seconds, max
  progress, resume, last view score, net feedback.
- **`subject_content_daily`** — one row per reference × subject × UTC day.
  Windows sum days per subject first, so unique viewers over 7/30/90/365/all
  days are exact and deleting a subject removes exactly its contributions.
- **`content_pairs`** — the co-engagement rollup (work-level), rebuilt by
  `RefreshCoEngagement`.
- **`exposures`** — what was shown: one row per result list and stage
  (served/rendered/visible) with parallel `content_kinds`/`content_ids`/
  `content_version_ids`/`positions` arrays. `Attribution` joins canonical
  clicks to the requested stage.
- **`erasures`** — the permanent fence: `(tenant, sipHash128(kind:subject))`.

Projections are rebuilt from canonical signals for every touched key and
versioned by the newest raw ingest time, so a projection that saw more events
always replaces one that saw fewer. `RepairProjections` heals crash residue in
bounded, resumable steps.

## Reads

| Read | Scope |
|---|---|
| `States` | exact references given (work or version) |
| `Metrics` | exact references; a work never includes its version rows |
| `History`, `HistoryCount`, `SeenIDs`, `TopStates`, `NegativeIDs` | work-level |
| `Popular`, `PopularityFor`, `CoEngaged` | work-level, one kind |
| `Attribution` | renders and clicks at one stage |

Windows are whole UTC calendar days `[From, To)`; every event inside counts
equally. The default popularity rank is `log10(1 + viewers) × max(floor,
Bayesian mean view score)`; hosts may tune `RankWeights` or replace the
expression with `RankExpr` over the metric columns.

## Scorer

Hosts map a raw session to `Scored{Score, Progress, ProgressMax, Completed}`
per content kind: gallery pages read / page count, video watch %, post scroll
depth. The kit owns everything else.

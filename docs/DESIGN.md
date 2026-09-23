# ContentKit design notes

The binding design is
[open-rails-tracker/contentkit/DESIGN.md](https://github.com/open-rails/tracker/blob/master/contentkit/DESIGN.md)
and the 2026-09-17 platform design in the doujins-org tracker. This file
records what the code in this repository assumes, including the owner
clarification of 2026-09-17: semantic search stays entirely outside ContentKit.

## Two libraries

- **ContentKit** (this repo) is deterministic: keyword search, the signal
  plane, discovery reads, and — after C2/C3 — comments, reactions, favorites,
  polls and the durable preference outbox. It links no model provider.
- **User Intelligence** is a separate, deferred library that owns semantic
  search and its own tables. ContentKit has no semantic runtime hook.
- **DocumentSink** is a neutral document change feed for external indexes,
  caches and audit consumers; no consumer implementation is linked here.

## Mechanism vs meaning

ContentKit owns storage, indexing, aggregation and the queries. Content kinds,
signal types, scoring weights, completion rules and visibility are host data:
no business noun appears in a schema. Hosts supply a `Scorer` per kind, a
`ContentCatalog` for the unseen universe and an `Eligibility` join for search.

## Tenant is a key

Every table, index, cursor and API carries the tenant. The embedded hub and
client pin one tenant at construction; the signal store validates every
reference against the tenant of the call. Nothing is defaulted.

## Storage

- Postgres, host schema: `content_search_documents`, `content_search_dirty`,
  `content_search_backfill` (keyword profile). Interaction tables (`content_posts`, `content_comments`, ...) arrive with C2.
- ClickHouse, dedicated database: `signals` (canonical, versioned), the
  rebuildable `subject_content_state` and `subject_content_daily`
  projections, `content_pairs`, `exposures`, `erasures`. See
  [signal-plane.md](signal-plane.md).

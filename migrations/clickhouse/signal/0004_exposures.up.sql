-- parent: 3 sha256:dc3984603a0c8aa4e48bbaff99cb80a4e99d94ec4bd12ca3bb1bad63b256a979
-- Result-list exposures for evaluation (#881). One row per rendered list and
-- observation stage (served < rendered < visible); a stage's item list is
-- cumulative and replaced by higher revisions. No query text is stored.
-- Identity (tenant, render_id, stage); version = revision << 64 | content hash.
-- The legacy search_impressions table is no longer read or written; it stays
-- (covered by erasure) until a later migration drops it.

CREATE TABLE IF NOT EXISTS exposures {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    render_id String,
    stage LowCardinality(String),
    revision UInt64 DEFAULT 0,
    query_id String DEFAULT '',
    surface LowCardinality(String),
    ranker LowCardinality(String) DEFAULT '',
    language LowCardinality(String) DEFAULT '',
    subject_kind LowCardinality(String),
    subject String,
    entity_types Array(LowCardinality(String)),
    entity_ids Array(String),
    positions Array(UInt32),
    occurred_at DateTime('UTC'),
    version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(query_id, surface, ranker, language, subject_kind, subject, entity_types, entity_ids, positions, occurred_at))),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    INDEX idx_exposures_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, render_id, stage)
SETTINGS index_granularity = 8192;

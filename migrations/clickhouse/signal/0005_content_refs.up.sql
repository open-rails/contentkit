-- parent: 4 sha256:55f6c6d8d9fcc0aba3895fff14d26404d0f43a9ce11f43eb0305d22af79b11b2
-- ContentKit hard cut (contentkit #1): every signal is keyed by a tenant-scoped
-- content reference (content_kind, content_id, content_version_id; '' = the
-- work itself). ClickHouse cannot rename sorting-key columns, so the re-keyed
-- tables are recreated under their ContentKit names and copied; exposures
-- converts in place. The pre-ContentKit tables (events, subject_state,
-- subject_daily, item_pairs) are neither read nor written any more and stay
-- until a later migration drops them after the checksum in docs/migration.md.

CREATE TABLE IF NOT EXISTS signals {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String DEFAULT '',
    subject_kind LowCardinality(String),
    subject String,
    signal_type LowCardinality(String),
    event_id String,
    revision UInt64 DEFAULT 0,
    occurred_at DateTime('UTC'),
    duration_s UInt32 DEFAULT 0,
    progress UInt32 DEFAULT 0,
    progress_max UInt32 DEFAULT 0,
    value Float64 DEFAULT 0,
    score Int16 DEFAULT 0,
    completed Bool DEFAULT false,
    resume String DEFAULT '',
    payload String DEFAULT '',
    version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(occurred_at, duration_s, progress, progress_max, value, score, completed, resume, payload))),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    INDEX idx_signals_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4,
    INDEX idx_signals_ingested ingested_at TYPE minmax GRANULARITY 1
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id)
SETTINGS index_granularity = 8192;

INSERT INTO signals (tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, revision,
    occurred_at, duration_s, progress, progress_max, value, score, completed, resume, payload, ingested_at)
SELECT tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, revision,
    occurred_at, duration_s, progress, progress_max, value, score, completed, resume, payload, ingested_at
FROM events;

CREATE TABLE IF NOT EXISTS subject_content_state {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_kind LowCardinality(String),
    subject String,
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String DEFAULT '',
    first_seen_at DateTime('UTC'),
    last_signal_at DateTime('UTC'),
    total_events UInt32,
    views UInt32,
    completions UInt32,
    active_s UInt64,
    max_progress UInt32,
    progress_max UInt32,
    completed Bool,
    resume String,
    last_score Int16,
    net_value Float64,
    feedback UInt32,
    version DateTime64(6, 'UTC')
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
ORDER BY (tenant, subject_kind, subject, content_kind, content_id, content_version_id)
SETTINGS index_granularity = 8192;

INSERT INTO subject_content_state (tenant, subject_kind, subject, content_kind, content_id, first_seen_at, last_signal_at,
    total_events, views, completions, active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback, version)
SELECT tenant, subject_kind, subject, entity_type, entity_id, first_seen_at, last_signal_at,
    total_events, views, completions, active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback, version
FROM subject_state;

CREATE TABLE IF NOT EXISTS subject_content_daily {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String DEFAULT '',
    subject_kind LowCardinality(String),
    subject String,
    day Date,
    events UInt32,
    views UInt32,
    completions UInt32,
    active_s UInt64,
    score_sum Int64,
    value_sum Float64,
    type_counts Map(LowCardinality(String), UInt32),
    version DateTime64(6, 'UTC'),
    INDEX idx_subject_content_daily_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, content_id, content_version_id, subject_kind, subject, day)
SETTINGS index_granularity = 8192;

INSERT INTO subject_content_daily (tenant, content_kind, content_id, subject_kind, subject, day, events, views, completions,
    active_s, score_sum, value_sum, type_counts, version)
SELECT tenant, entity_type, entity_id, subject_kind, subject, day, events, views, completions,
    active_s, score_sum, value_sum, type_counts, version
FROM subject_daily;

CREATE TABLE IF NOT EXISTS content_pairs {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind_a LowCardinality(String),
    content_id_a String,
    content_kind_b LowCardinality(String),
    content_id_b String,
    strength Int64,
    refreshed_at DateTime('UTC') DEFAULT now()
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', refreshed_at)
ORDER BY (tenant, content_kind_a, content_id_a, content_kind_b, content_id_b)
SETTINGS index_granularity = 8192;

INSERT INTO content_pairs (tenant, content_kind_a, content_id_a, content_kind_b, content_id_b, strength, refreshed_at)
SELECT tenant, entity_type_a, entity_id_a, entity_type_b, entity_id_b, strength, refreshed_at
FROM item_pairs;

ALTER TABLE exposures {{ON_CLUSTER}} RENAME COLUMN IF EXISTS entity_types TO content_kinds;

ALTER TABLE exposures {{ON_CLUSTER}} RENAME COLUMN IF EXISTS entity_ids TO content_ids;

ALTER TABLE exposures {{ON_CLUSTER}} ADD COLUMN IF NOT EXISTS content_version_ids Array(String) AFTER content_ids;

ALTER TABLE exposures {{ON_CLUSTER}} MODIFY COLUMN version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(query_id, surface, ranker, language, subject_kind, subject, content_kinds, content_ids, content_version_ids, positions, occurred_at)));

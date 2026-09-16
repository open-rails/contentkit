-- parent: 1 sha256:1832a7dc9dac309738fc7998e6cc91f221f4dce1cc70c6e778b183aa0de057c0
-- Canonical event identity and rebuildable compact projections (#874, #878).
-- events: one logical event per (tenant, entity, subject, signal_type, event_id);
--   the highest version wins, where version = revision << 64 | content hash, so
--   retries, reordering and merges resolve identically.
-- subject_state: indefinite per subject x entity state.
-- subject_daily: indefinite per subject x entity x UTC day contributions; the
--   source for popularity windows, exact unique viewers and per-subject deletion.
-- Both projections are versioned by the newest raw row they were derived from.
-- Legacy signal_events rows are copied (revision 0) and kept until a later
-- migration after projections are rebuilt with Store.RepairProjections.

CREATE TABLE IF NOT EXISTS events {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    entity_type LowCardinality(String),
    entity_id String,
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
    INDEX idx_events_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4,
    INDEX idx_events_ingested ingested_at TYPE minmax GRANULARITY 1
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS subject_state {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_kind LowCardinality(String),
    subject String,
    entity_type LowCardinality(String),
    entity_id String,
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
ORDER BY (tenant, subject_kind, subject, entity_type, entity_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS subject_daily {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    entity_type LowCardinality(String),
    entity_id String,
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
    INDEX idx_subject_daily_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, entity_type, entity_id, subject_kind, subject, day)
SETTINGS index_granularity = 8192;

INSERT INTO events (tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, revision,
    occurred_at, duration_s, progress, progress_max, value, score, completed, resume, payload)
SELECT tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, 0,
    occurred_at, duration_s, progress, progress_max, value, score, completed, resume, payload
FROM signal_events;

DROP VIEW IF EXISTS mv_entity_daily {{ON_CLUSTER}} SYNC;

DROP TABLE IF EXISTS entity_daily {{ON_CLUSTER}} SYNC;

DROP TABLE IF EXISTS signal_state {{ON_CLUSTER}} SYNC;

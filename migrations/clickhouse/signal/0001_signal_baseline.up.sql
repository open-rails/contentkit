-- parent: root
-- Signal-plane baseline: the exact tables previously created at host startup
-- by signal.EnsureSchema (replicated form). Existing installations record this
-- migration without changes; later migrations own every schema change.
-- Apply with migratekit chmigrate against the dedicated signal database.

CREATE TABLE IF NOT EXISTS signal_events {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    entity_type LowCardinality(String),
    entity_id String,
    subject_kind LowCardinality(String),
    subject String,
    signal_type LowCardinality(String),
    event_id String,
    occurred_at DateTime('UTC'),
    duration_s UInt32 DEFAULT 0,
    progress UInt32 DEFAULT 0,
    progress_max UInt32 DEFAULT 0,
    value Float64 DEFAULT 0,
    label LowCardinality(String) DEFAULT '',
    weight Float64 DEFAULT 1,
    score Int16 DEFAULT 0,
    completed Bool DEFAULT false,
    resume String DEFAULT '',
    payload String DEFAULT '',
    recorded_at DateTime('UTC') DEFAULT now(),
    INDEX idx_signal_events_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', recorded_at)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, entity_type, entity_id, subject_kind, subject, occurred_at, event_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS signal_state {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_kind LowCardinality(String),
    subject String,
    entity_type LowCardinality(String),
    entity_id String,
    first_seen_at DateTime('UTC'),
    last_signal_at DateTime('UTC'),
    total_events UInt32 DEFAULT 0,
    max_progress UInt32 DEFAULT 0,
    progress_max UInt32 DEFAULT 0,
    completed Bool DEFAULT false,
    resume String DEFAULT '',
    has_interacted Bool DEFAULT false,
    last_score Int16 DEFAULT 0,
    net_value Float64 DEFAULT 0,
    last_updated DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', last_updated)
ORDER BY (tenant, subject_kind, subject, entity_type, entity_id)
SETTINGS index_granularity = 8192;

-- Installations created before net_value existed.
ALTER TABLE signal_state {{ON_CLUSTER}} ADD COLUMN IF NOT EXISTS net_value Float64 DEFAULT 0 AFTER last_score;

CREATE TABLE IF NOT EXISTS entity_daily {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    entity_type LowCardinality(String),
    entity_id String,
    day Date,
    subjects AggregateFunction(uniqExact, String),
    signals SimpleAggregateFunction(sum, UInt64),
    engagement_sum SimpleAggregateFunction(sum, Int64),
    scored_signals SimpleAggregateFunction(sum, UInt64),
    completions SimpleAggregateFunction(sum, UInt64),
    value_sum SimpleAggregateFunction(sum, Float64),
    signal_counts SimpleAggregateFunction(sumMap, Map(String, UInt64))
) ENGINE = ReplicatedAggregatingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
PARTITION BY toYear(day)
ORDER BY (tenant, entity_type, entity_id, day)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS item_pairs {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    entity_type_a LowCardinality(String),
    entity_id_a String,
    entity_type_b LowCardinality(String),
    entity_id_b String,
    strength Int64,
    refreshed_at DateTime('UTC') DEFAULT now()
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', refreshed_at)
ORDER BY (tenant, entity_type_a, entity_id_a, entity_type_b, entity_id_b)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS search_impressions {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    query_id String,
    surface LowCardinality(String) DEFAULT 'search',
    normalized_query String DEFAULT '',
    language LowCardinality(String) DEFAULT '',
    subject_kind LowCardinality(String) DEFAULT '',
    subject String DEFAULT '',
    shown_entity_types Array(LowCardinality(String)),
    shown_entity_ids Array(String),
    shown_positions Array(UInt32),
    occurred_at DateTime('UTC'),
    recorded_at DateTime('UTC') DEFAULT now()
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', recorded_at)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, occurred_at, query_id)
SETTINGS index_granularity = 8192;

CREATE MATERIALIZED VIEW IF NOT EXISTS mv_entity_daily {{ON_CLUSTER}} TO entity_daily AS
SELECT
    tenant,
    entity_type,
    entity_id,
    toDate(occurred_at) AS day,
    uniqExactState(concat(subject_kind, ':', subject)) AS subjects,
    toUInt64(count()) AS signals,
    toInt64(sum(score)) AS engagement_sum,
    toUInt64(countIf(score != 0)) AS scored_signals,
    toUInt64(countIf(completed)) AS completions,
    sum(value) AS value_sum,
    sumMap(map(signal_type, toUInt64(1))) AS signal_counts
FROM signal_events
GROUP BY tenant, entity_type, entity_id, day;

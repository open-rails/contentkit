-- ContentKit's signal plane. The host selects its ClickHouse database.
-- content_version_id '' is the work itself, as NULL is in Postgres: it stays
-- out of Nullable because it is in every sorting key.

CREATE TABLE IF NOT EXISTS engagement_daily {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `content_id` String,
    `content_version_id` String,
    `subject_kind` LowCardinality(String),
    `subject` String,
    `day` Date,
    `present` Bool,
    `active_ms` UInt64,
    `covered_units` Array(UInt32),
    `unit_attention` Map(UInt32, UInt64),
    `unit_total` UInt32,
    `unit_scale` UInt32,
    `version` UInt256,
    INDEX idx_engagement_daily_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, content_id, content_version_id, subject_kind, subject, day)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS engagement_segments {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `content_id` String,
    `content_version_id` String,
    `subject_kind` LowCardinality(String),
    `subject` String,
    `receipt_id` String,
    `revision` UInt64,
    `visit_id` String,
    `segment_id` String,
    `occurred_at` DateTime64(6, 'UTC'),
    `active_ms` UInt32,
    `covered_units` Array(UInt32),
    `unit_attention` Map(UInt32, UInt64),
    `unit_total` UInt32,
    `unit_scale` UInt32,
    `resume` String,
    `access_context` String,
    `measurement` String,
    `version` UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(visit_id, segment_id, occurred_at, active_ms, covered_units, unit_attention, unit_total, unit_scale, resume, access_context, measurement, origin_click_id))),
    `ingested_at` DateTime64(6, 'UTC') DEFAULT now64(6),
    `origin_click_id` String DEFAULT '',
    INDEX idx_engagement_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, content_kind, content_id, content_version_id, subject_kind, subject, receipt_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS engagement_state {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject_kind` LowCardinality(String),
    `subject` String,
    `content_kind` LowCardinality(String),
    `content_id` String,
    `content_version_id` String,
    `source_version` String,
    `first_engaged_at` DateTime64(6, 'UTC'),
    `last_engaged_at` DateTime64(6, 'UTC'),
    `active_ms` UInt64,
    `covered_units` Array(UInt32),
    `unit_attention` Map(UInt32, UInt64),
    `unit_total` UInt32,
    `unit_scale` UInt32,
    `qualified` Bool,
    `coverage` Float64,
    `quality` Float64,
    `completed` Bool,
    `resume` String,
    `version` UInt256
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
ORDER BY (tenant, subject_kind, subject, content_kind, content_id, content_version_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS erasures {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject_hash` FixedString(16),
    `erased_at` DateTime64(6, 'UTC')
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', erased_at)
ORDER BY (tenant, subject_hash)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS exposures {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `render_id` String,
    `stage` LowCardinality(String),
    `revision` UInt64 DEFAULT 0,
    `query_id` String DEFAULT '',
    `surface` LowCardinality(String),
    `ranker` LowCardinality(String) DEFAULT '',
    `language` LowCardinality(String) DEFAULT '',
    `subject_kind` LowCardinality(String),
    `subject` String,
    `content_kinds` Array(LowCardinality(String)),
    `content_ids` Array(String),
    `content_version_ids` Array(String),
    `positions` Array(UInt32),
    `occurred_at` DateTime('UTC'),
    `version` UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(query_id, surface, ranker, language, subject_kind, subject, content_kinds, content_ids, content_version_ids, positions, occurred_at, access_contexts, search_query))),
    `ingested_at` DateTime64(6, 'UTC') DEFAULT now64(6),
    `access_contexts` Array(String) DEFAULT [],
    `search_query` String DEFAULT '',
    INDEX idx_exposures_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, render_id, stage)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS legacy_reading_state {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject_kind` LowCardinality(String),
    `subject` String,
    `content_kind` LowCardinality(String),
    `content_id` String,
    `content_version_id` String,
    `first_seen_at` DateTime('UTC'),
    `last_seen_at` DateTime('UTC'),
    `progress` UInt32,
    `progress_max` UInt32,
    `completed` Bool,
    `resume` String,
    `version` UInt256
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
ORDER BY (tenant, subject_kind, subject, content_kind, content_id, content_version_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS measured_population_coverage {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `started_at` SimpleAggregateFunction(min, DateTime64(6, 'UTC'))
)
ENGINE = ReplicatedAggregatingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
ORDER BY (tenant, content_kind, domain, language)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS measured_population_observations {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject_kind` LowCardinality(String),
    `subject` String,
    `content_kind` LowCardinality(String),
    `observation_id` String,
    `observed_at` DateTime64(6, 'UTC'),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `eligible_tiers` Array(String),
    `eligibility_known` Bool,
    `version` UInt64 DEFAULT cityHash64(observed_at, domain, language, eligible_tiers, eligibility_known),
    INDEX idx_population_observed observed_at TYPE minmax GRANULARITY 1
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(observed_at)
ORDER BY (tenant, content_kind, subject_kind, subject, observation_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS popularity_daily {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `day` Date,
    `content_id` String,
    `readers` UInt64,
    `eligible` UInt64,
    `version` UInt64
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, domain, language, day, content_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS popularity_daily_prepared {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `day` Date,
    `computed_at` DateTime64(6, 'UTC')
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', computed_at)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, domain, language, day)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS signals {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `content_id` String,
    `content_version_id` String DEFAULT '',
    `subject_kind` LowCardinality(String),
    `subject` String,
    `signal_type` LowCardinality(String),
    `event_id` String,
    `revision` UInt64 DEFAULT 0,
    `occurred_at` DateTime('UTC'),
    `value` Float64 DEFAULT 0,
    `payload` String DEFAULT '',
    `version` UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(occurred_at, value, payload))),
    `ingested_at` DateTime64(6, 'UTC') DEFAULT now64(6),
    INDEX idx_signals_subject (subject_kind, subject) TYPE bloom_filter GRANULARITY 4,
    INDEX idx_signals_ingested ingested_at TYPE minmax GRANULARITY 1
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id)
SETTINGS index_granularity = 8192;

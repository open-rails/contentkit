-- A clean identity lineage: old per-tab observations are never copied.
-- Independent, trusted activity observations for event-time audience sizes.
CREATE TABLE IF NOT EXISTS measured_population_observations {{ON_CLUSTER}} (
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

-- Non-personal instrumentation watermark; account erasure must not move the
-- start of trusted history or silently change the ranking policy.
CREATE TABLE IF NOT EXISTS measured_population_coverage {{ON_CLUSTER}} (
    `tenant` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `started_at` SimpleAggregateFunction(min, DateTime64(6, 'UTC'))
)
ENGINE = ReplicatedAggregatingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
ORDER BY (tenant, content_kind, domain, language);

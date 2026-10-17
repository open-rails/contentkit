-- Additive while old and new app writers overlap. Old counter tables remain
-- untouched until the explicit post-cohort retirement operation runs.
CREATE TABLE IF NOT EXISTS engagement_segments {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String,
    subject_kind LowCardinality(String),
    subject String,
    receipt_id String,
    revision UInt64,
    visit_id String,
    segment_id String,
    occurred_at DateTime64(6, 'UTC'),
    active_ms UInt32,
    covered_units Array(UInt32),
    unit_attention Map(UInt32, UInt64),
    unit_total UInt32,
    unit_scale UInt32,
    resume String,
    access_context String,
    measurement String,
    version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision),64),toUInt128(cityHash64(visit_id,segment_id,occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement))),
    ingested_at DateTime64(6, 'UTC') DEFAULT now64(6),
    INDEX idx_engagement_subject (subject_kind,subject) TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS engagement_state {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_kind LowCardinality(String),
    subject String,
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String,
    source_version String,
    first_engaged_at DateTime64(6, 'UTC'),
    last_engaged_at DateTime64(6, 'UTC'),
    active_ms UInt64,
    covered_units Array(UInt32),
    unit_attention Map(UInt32, UInt64),
    unit_total UInt32,
    unit_scale UInt32,
    qualified Bool,
    coverage Float64,
    quality Float64,
    completed Bool,
    resume String,
    version UInt256
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
ORDER BY (tenant,subject_kind,subject,content_kind,content_id,content_version_id)
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS engagement_daily {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String,
    subject_kind LowCardinality(String),
    subject String,
    day Date,
    present Bool,
    active_ms UInt64,
    covered_units Array(UInt32),
    unit_attention Map(UInt32, UInt64),
    unit_total UInt32,
    unit_scale UInt32,
    version UInt256,
    INDEX idx_engagement_daily_subject (subject_kind,subject) TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant,content_kind,content_id,content_version_id,subject_kind,subject,day)
SETTINGS index_granularity = 8192;

-- Preserve useful history without copying any old opens/session counters or
-- treating old route progress as measured attention. Retirement refreshes this
-- copy once more after the final old writer stops.
CREATE TABLE IF NOT EXISTS legacy_reading_state {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_kind LowCardinality(String),
    subject String,
    content_kind LowCardinality(String),
    content_id String,
    content_version_id String,
    first_seen_at DateTime('UTC'),
    last_seen_at DateTime('UTC'),
    progress UInt32,
    progress_max UInt32,
    completed Bool,
    resume String,
    version UInt256
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
ORDER BY (tenant,subject_kind,subject,content_kind,content_id,content_version_id)
SETTINGS index_granularity = 8192;

INSERT INTO legacy_reading_state
SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
    first_seen_at,last_view_at,max_progress,progress_max,completed,resume,
    bitOr(bitShiftLeft(toUInt256(toUnixTimestamp64Micro(toDateTime64(last_view_at,6,'UTC'))),128),toUInt256(cityHash64(first_seen_at,last_view_at,max_progress,progress_max,completed,resume)))
FROM subject_content_state FINAL
WHERE max_progress>0 AND (sipHash128(concat(subject_kind,':',subject)),tenant)
    NOT IN (SELECT subject_hash,tenant FROM erasures);

-- Co-engagement derived from measured readers has a separate lineage too.
-- Old writers may continue refreshing content_pairs during the rolling update;
-- those old opens must never reappear as new recommendation evidence.
CREATE TABLE IF NOT EXISTS engagement_pairs {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind_a LowCardinality(String),
    content_id_a String,
    content_kind_b LowCardinality(String),
    content_id_b String,
    strength Int64,
    refreshed_at DateTime('UTC') DEFAULT now()
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', refreshed_at)
ORDER BY (tenant,content_kind_a,content_id_a,content_kind_b,content_id_b)
SETTINGS index_granularity = 8192;

-- Reusable anonymous item/day participation. Canonical measurements and
-- population observations remain the source for explicit historical rebuilds.
CREATE TABLE IF NOT EXISTS popularity_daily {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    domain LowCardinality(String),
    language LowCardinality(String),
    day Date,
    content_id String,
    readers UInt64,
    eligible UInt64,
    version UInt64
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', version)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, domain, language, day, content_id);

-- Successful empty days need a checkpoint too. There is no per-reader ledger
-- or automatic repair job; the existing ranking refresh prepares recent days.
CREATE TABLE IF NOT EXISTS popularity_daily_prepared {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    content_kind LowCardinality(String),
    domain LowCardinality(String),
    language LowCardinality(String),
    day Date,
    computed_at DateTime64(6, 'UTC')
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', computed_at)
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, content_kind, domain, language, day);

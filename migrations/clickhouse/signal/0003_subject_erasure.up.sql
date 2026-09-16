-- parent: 2 sha256:eca60e6d64cc50abd2af25c8d2604d54243fe112e36c5f966904c39d5f512f0d
-- Subject erasure ledger (#877): the ingestion fence and deletion barrier.
-- One row per erased tenant × subject; only a hash of the subject is kept
-- (anonymous keys are bearer capabilities). Rows are permanent: writes,
-- impressions and projection rebuilds for a fenced subject are dropped.

CREATE TABLE IF NOT EXISTS erasures {{ON_CLUSTER}} (
    tenant LowCardinality(String),
    subject_hash FixedString(16),
    erased_at DateTime64(6, 'UTC')
) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', erased_at)
ORDER BY (tenant, subject_hash)
SETTINGS index_granularity = 8192;

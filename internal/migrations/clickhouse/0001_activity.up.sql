-- ContentKit's activity log and its daily rollups. The host selects the database.
-- A subject is the account id when signed in, otherwise the anonymous id the
-- engine derives from the browser's; the zero UUID is "none" in UUID columns.

-- Every action a person takes, as the API received it. Raw personal rows live
-- 90 days: whole days expire, and a day's queries read one partition.
CREATE TABLE IF NOT EXISTS activity {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject` UUID,
    `ts` DateTime64(3, 'UTC'),
    -- open, progress, reaction, favorite, hide, search
    `action` LowCardinality(String),
    `content_kind` LowCardinality(String),
    `content_id` UUID,
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    -- anonymous, account, or the account's claimed tiers joined by commas
    `access` LowCardinality(String),
    -- local time minus UTC, in minutes
    `utc_offset` Int16,
    -- open: the list it came from
    `request_id` UUID,
    `surface` LowCardinality(String),
    `position` UInt16,
    -- progress: active time since the previous report; furthest and current
    -- page or second; the length in the same unit
    `active_ms` UInt32,
    `furthest` UInt32,
    `current` UInt32,
    `length` UInt32,
    `unit` LowCardinality(String),
    -- reaction (1, -1, 0), favorite, hide (1, 0) and a reason when one was given
    `value` Int8,
    `reason` LowCardinality(String),
    -- search: the normalized query and the results in order
    `query` String,
    `results` Array(UUID),
    `ingested_at` DateTime64(3, 'UTC') DEFAULT now64(3),
    INDEX idx_activity_ingested ingested_at TYPE minmax GRANULARITY 1
)
ENGINE = ReplicatedMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
PARTITION BY toDate(ts)
ORDER BY (tenant, subject, ts)
TTL toDate(ts) + toIntervalDay(90)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

-- People per gallery × site × day by read class and the day's last reaction,
-- split by whether the reader is in the gallery's audience. Nothing personal;
-- kept long-term. A day is rebuilt whole: readers take each day's newest
-- generation, and older generations are deleted after it lands.
CREATE TABLE IF NOT EXISTS content_daily {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `day` Date,
    `content_kind` LowCardinality(String),
    `content_id` UUID,
    `audience` Bool,
    -- '' (a reaction only), bounce, neutral, good, complete
    `class` LowCardinality(String),
    -- '', like, dislike, favorite, hidden
    `reaction` LowCardinality(String),
    `reason` LowCardinality(String),
    `people` UInt32,
    `generation` DateTime64(3, 'UTC')
)
ENGINE = ReplicatedMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, domain, language, day, content_kind, content_id)
SETTINGS index_granularity = 8192;

-- Exact active people per site × access × day: the denominator for gated
-- galleries. Rebuilt whole per day as content_daily is.
CREATE TABLE IF NOT EXISTS audience_daily {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `domain` LowCardinality(String),
    `language` LowCardinality(String),
    `day` Date,
    `access` LowCardinality(String),
    `people` UInt32,
    `generation` DateTime64(3, 'UTC')
)
ENGINE = ReplicatedMergeTree('/clickhouse/tables/{database}/{table}', '{replica}')
PARTITION BY toYYYYMM(day)
ORDER BY (tenant, domain, language, day, access)
SETTINGS index_granularity = 8192;

-- Erased subjects: every read of activity filters them, and a daily
-- lightweight DELETE removes their rows.
CREATE TABLE IF NOT EXISTS erasures {{ON_CLUSTER}}
(
    `tenant` LowCardinality(String),
    `subject` UUID,
    `erased_at` DateTime64(3, 'UTC')
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', erased_at)
ORDER BY (tenant, subject)
SETTINGS index_granularity = 8192;

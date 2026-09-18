-- parent: 3 sha256:0fb9222a6889ae48d0ba27c6f35f3cb49b9574bb1af26fc42a799b7a1d5fa7d8
-- Durable preference boundary: one compact snapshot row per
-- (tenant, actor, canonical content reference, axis), written in the SAME
-- transaction as the reaction/favorite it reflects. `revision` comes from a
-- schema-local sequence allocated after the key's advisory lock (never
-- wall-clock, never callback arrival), so it orders one actor's mutations
-- across processes and restarts. Neutral/unfavorite keep a zero-valued
-- snapshot so a delayed like cannot resurrect removed state.
-- `delivered_revision` is the sink's revision-specific acknowledgement; a
-- newer mutation leaves the row pending. CACHE 1: a cached block would let one
-- backend hand out a numerically older revision after another backend
-- committed a newer mutation.
CREATE SEQUENCE content_preference_revision_seq AS bigint INCREMENT 1 MINVALUE 1 NO CYCLE CACHE 1;

CREATE TABLE content_preference_snapshots (
    tenant_id          text        NOT NULL,
    actor_id           text        NOT NULL,
    content_kind       text        NOT NULL,
    content_id         text        NOT NULL,
    content_version_id text        NOT NULL DEFAULT '',
    axis               text        NOT NULL,
    value              smallint    NOT NULL,
    revision           bigint      NOT NULL,
    occurred_at        timestamptz NOT NULL,
    delivered_revision bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, actor_id, content_kind, content_id, content_version_id, axis),
    CONSTRAINT content_preference_snapshots_axis_ck CHECK (axis IN ('reaction', 'favorite')),
    CONSTRAINT content_preference_snapshots_value_ck CHECK (value IN (-1, 0, 1) AND (axis <> 'favorite' OR value IN (0, 1))),
    CONSTRAINT content_preference_snapshots_revision_ck CHECK (revision > 0 AND delivered_revision >= 0 AND delivered_revision <= revision)
);
-- Pending sweep in key order (keyset paging on the primary key).
CREATE INDEX content_preference_snapshots_pending_idx
    ON content_preference_snapshots (tenant_id, actor_id, content_kind, content_id, content_version_id, axis)
    WHERE delivered_revision < revision;

-- Pre-collapse source rows kept as cutover evidence when per-language
-- preference rows are reconciled onto canonical references (MigratePreferences).
CREATE TABLE content_preference_key_archive (
    archived_at        timestamptz NOT NULL DEFAULT now(),
    tenant_id          text        NOT NULL,
    axis               text        NOT NULL,
    actor_id           text,
    ip                 text,
    content_kind       text        NOT NULL,
    content_id         text        NOT NULL,   -- the original, pre-collapse id
    content_version_id text        NOT NULL,
    value              smallint    NOT NULL,
    source_at          timestamptz NOT NULL
);

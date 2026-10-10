-- parent: 11 sha256:f7329ee9ffcb77f5b19cd75343e6f69319271ff8f48c4528c1d8151df6d7df04

-- Each item's current public publications, projected from its S3 manifest in
-- the journal transaction that settles the write making them current. Hosts
-- resolve a page of card, avatar or inline image URLs with one indexed query.
-- The key's prefix serves both an item's images and one upload's by {name}.
CREATE TABLE content_media_publications (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    upload_name text NOT NULL,
    upload_path text NOT NULL,
    preset text NOT NULL,
    ordinal integer NOT NULL,
    generation uuid, -- NULL: legacy files at their logical names
    renditions jsonb NOT NULL, -- [{name, w, h}]: physical names in public/ and encoded sizes
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_kind, content_id, upload_name, upload_path, preset),
    CHECK (tenant_id <> '' AND content_kind <> '' AND content_id <> '' AND upload_path <> '' AND preset <> ''),
    CHECK (ordinal >= 0 AND jsonb_typeof(renditions) = 'array' AND jsonb_array_length(renditions) > 0)
);

-- The media upgrade's pass over each kind's folders: items stored before
-- v0.68 are converted in place, every current item is projected.
CREATE TABLE content_media_upgrades (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    after_id text NOT NULL DEFAULT '',
    visited bigint NOT NULL DEFAULT 0,
    upgraded bigint NOT NULL DEFAULT 0,
    finished_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_kind),
    CHECK (tenant_id <> '' AND content_kind <> '' AND visited >= 0 AND upgraded >= 0)
);

-- Items the upgrade left as they were, retried on every run.
CREATE TABLE content_media_upgrade_failures (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    error text NOT NULL,
    failed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_kind, content_id)
);

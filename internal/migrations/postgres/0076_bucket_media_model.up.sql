-- Each item's manifest.json in the bucket is the only record of its files.
-- The journal, allocations, cleanup queue, processing and publication
-- projections, publication requests and the unused quota ledger go; one
-- rebuildable cache row per item remains (media-worker rebuild). The
-- account lineage makes the same change after this one.
DROP INDEX IF EXISTS content_versions_release_idx;
ALTER TABLE content_versions DROP COLUMN IF EXISTS release_id;
-- A purge claims a version's erasure here, under its row lock, before any
-- storage call: a restore refuses a claimed version.
ALTER TABLE content_versions ADD COLUMN IF NOT EXISTS purge_claimed_at timestamp with time zone;
DROP TABLE IF EXISTS content_publication_requests;
DROP TABLE IF EXISTS content_media_commits, content_media_allocations, content_media_cleanup, content_media_processing,
    content_media_publications, content_media_usage, content_media_reservations, content_media_releases, content_media_upload_rates;

-- accounts/0001 creates the shared event-log objects only where
-- content_media_commits is absent, and accounts/0003 indexes these columns.
-- In a schema both lineages share, the account lineage runs after this one:
-- this empty stand-in keeps its baseline from creating them twice.
-- accounts/0004 drops it.
CREATE TABLE IF NOT EXISTS content_media_commits (tenant_id text, actor_id text, effects jsonb);

CREATE TABLE IF NOT EXISTS content_media_items (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text DEFAULT ''::text NOT NULL,
    rev bigint NOT NULL,
    etag text NOT NULL,
    state text NOT NULL,
    status jsonb NOT NULL,
    images jsonb NOT NULL,
    deleted boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_items_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (rev > 0) AND (etag <> ''::text))),
    CONSTRAINT content_media_items_images_check CHECK ((jsonb_typeof(images) = 'array'::text)),
    CONSTRAINT content_media_items_status_check CHECK ((jsonb_typeof(status) = 'object'::text)),
    CONSTRAINT content_media_items_pkey PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id)
);

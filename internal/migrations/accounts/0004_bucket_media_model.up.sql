-- Account media (avatars) follows the bucket manifest model: its journal,
-- allocations, cleanup, projections and quota ledger go; one rebuildable
-- cache row per item remains. In a schema shared with the catalog this also
-- drops the catalog's empty content_media_commits stand-in.
DROP TABLE IF EXISTS content_media_commits, content_media_allocations, content_media_cleanup, content_media_processing,
    content_media_publications, content_media_usage, content_media_reservations, content_media_releases, content_media_upload_rates;

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

-- ContentKit #117 media: a content kind's media items are its versions,
-- {kind, content, version}, plus one content-level item; '' is no version.

ALTER TABLE content_media_publications ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE content_media_publications DROP CONSTRAINT content_media_publications_pkey;
ALTER TABLE content_media_publications
    ADD PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset);

ALTER TABLE content_media_commits ADD COLUMN content_version_id text NOT NULL DEFAULT '';

-- A version reaches viewers through publications: frozen snapshots of its
-- media copied into immutable items of the kind's published kind. A pages
-- publication has one item per span of pages; a playback publication is
-- itself the item. One is built beside the current one, which it replaces.
CREATE TABLE content_version_publications (
    tenant_id text NOT NULL,
    id text NOT NULL,
    version_id text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL DEFAULT 'building',
    source jsonb NOT NULL,
    previews integer, -- preview pages put in the version's item
    previews_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    ready_at timestamp with time zone,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES content_versions (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_version_publications_kind_check CHECK (kind IN ('pages', 'playback')),
    CONSTRAINT content_version_publications_state_check CHECK (state IN ('building', 'current'))
);

CREATE UNIQUE INDEX content_version_publications_state_idx ON content_version_publications (tenant_id, version_id, kind, state);
CREATE INDEX content_version_publications_building_idx ON content_version_publications (tenant_id, created_at) WHERE state = 'building';

-- One published item of a pages publication: a segment's pages (by its
-- position) or pages outside every segment.
CREATE TABLE content_version_publication_items (
    tenant_id text NOT NULL,
    id text NOT NULL,
    publication_id text NOT NULL,
    segment integer,
    start_index integer NOT NULL,
    end_index integer NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, publication_id) REFERENCES content_version_publications (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT content_version_publication_items_range_check CHECK (start_index >= 0 AND end_index > start_index)
);

CREATE INDEX content_version_publication_items_publication_idx ON content_version_publication_items (tenant_id, publication_id, start_index);

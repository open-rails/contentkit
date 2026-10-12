-- ContentKit #117 media: a content kind's media items are its versions,
-- {kind, content, version}, plus one content-level item; '' is no version.

ALTER TABLE content_media_publications ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE content_media_publications DROP CONSTRAINT content_media_publications_pkey;
ALTER TABLE content_media_publications
    ADD PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset);

ALTER TABLE content_media_commits ADD COLUMN content_version_id text NOT NULL DEFAULT '';

DROP INDEX content_media_commits_folder_owner;

CREATE INDEX content_media_commits_folder_owner ON content_media_commits USING btree (tenant_id, folder_prefix, content_kind, content_id, content_version_id);

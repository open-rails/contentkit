-- Build first, so reads keep the old index until the swap at the end.
CREATE INDEX content_media_commits_folder_owner_next ON content_media_commits USING btree (tenant_id, folder_prefix, content_kind, content_id, content_version_id);

DROP INDEX content_media_commits_folder_owner;

ALTER INDEX content_media_commits_folder_owner_next RENAME TO content_media_commits_folder_owner;

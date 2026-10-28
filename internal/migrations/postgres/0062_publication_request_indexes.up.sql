CREATE INDEX content_publication_requests_version_idx ON content_publication_requests USING btree (tenant_id, version_id);

CREATE INDEX content_versions_release_idx ON content_versions USING btree (tenant_id, release_id) WHERE (release_id IS NOT NULL);

CREATE INDEX content_comments_admin_idx ON content_comments USING btree (tenant_id, created_at DESC, id DESC);

CREATE INDEX content_reactions_user_idx ON content_reactions USING btree (tenant_id, user_id) WHERE (user_id IS NOT NULL);

CREATE INDEX content_comments_content_idx ON content_comments USING btree (tenant_id, content_kind, content_id, content_version_id);

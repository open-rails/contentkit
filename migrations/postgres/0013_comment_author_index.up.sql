
-- A person's comments for moderators, newest first: deleted, held and
-- rejected ones included (GET /comments/admin?author=).
CREATE INDEX content_comments_author_idx ON content_comments USING btree (tenant_id, user_id, created_at DESC, id DESC) WHERE (user_id IS NOT NULL);

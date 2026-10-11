-- parent: 12 sha256:4a79a752ce3b646fad15fe0a251799b84e46e3aa390f8977764ecdcb02ce7362

-- A person's comments for moderators, newest first: deleted, held and
-- rejected ones included (GET /comments/admin?author=).
CREATE INDEX content_comments_author_idx ON content_comments USING btree (tenant_id, user_id, created_at DESC, id DESC) WHERE (user_id IS NOT NULL);

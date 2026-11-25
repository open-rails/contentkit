-- migratekit:no-transaction
-- Copies content_favorites into content_favorites_next in batches that each
-- commit, rewrites the keys written meanwhile, then indexes the copy. Writers
-- of content_favorites never wait on it. Rerunnable from the start.
DROP INDEX IF EXISTS content_favorites_next_user_key;
DROP INDEX IF EXISTS content_favorites_next_content_idx;
DROP INDEX IF EXISTS content_favorites_next_revision_idx;
DROP INDEX IF EXISTS content_favorites_next_user_created_idx;
DROP INDEX IF EXISTS content_favorites_next_popular_idx;
CALL content_favorites_copy();
CALL content_favorites_settle();
-- Toggle and card state.
CREATE UNIQUE INDEX content_favorites_next_user_key ON content_favorites_next
    (app_id, user_id, content_kind, content_id, content_version_id) NULLS NOT DISTINCT WHERE user_id IS NOT NULL;
-- Recount.
CREATE INDEX content_favorites_next_content_idx ON content_favorites_next (app_id, content_kind, content_id, content_version_id);
-- Preference sync.
CREATE INDEX content_favorites_next_revision_idx ON content_favorites_next (app_id, revision);
-- The reader's favorites, newest first.
CREATE INDEX content_favorites_next_user_created_idx ON content_favorites_next (app_id, user_id, created_at DESC) WHERE value = 1;
-- Entity rankings.
CREATE INDEX content_favorites_next_popular_idx ON content_favorites_next (app_id, content_kind, created_at)
    INCLUDE (user_id, content_id) WHERE value = 1 AND user_id IS NOT NULL;

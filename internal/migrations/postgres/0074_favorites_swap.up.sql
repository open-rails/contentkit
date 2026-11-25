-- Rewrites the keys written since 0073 while content_favorites is locked, then
-- puts the copy in its place. The lock lasts as long as those few keys take;
-- a writer or long read holding it past lock_timeout fails this step, which
-- runs again on the next migration.
SET LOCAL lock_timeout = '10s';
LOCK TABLE content_favorites IN ACCESS EXCLUSIVE MODE;
CALL content_favorites_settle();
DROP TABLE content_favorites;
DROP TABLE content_favorites_moved;
DROP PROCEDURE content_favorites_copy();
DROP PROCEDURE content_favorites_settle();
DROP FUNCTION content_favorites_note();
ALTER TABLE content_favorites_next RENAME TO content_favorites;
ALTER INDEX content_favorites_next_user_key RENAME TO content_favorites_user_key;
ALTER INDEX content_favorites_next_content_idx RENAME TO content_favorites_content_idx;
ALTER INDEX content_favorites_next_revision_idx RENAME TO content_favorites_revision_idx;
ALTER INDEX content_favorites_next_user_created_idx RENAME TO content_favorites_user_created_idx;
ALTER INDEX content_favorites_next_popular_idx RENAME TO content_favorites_popular_idx;

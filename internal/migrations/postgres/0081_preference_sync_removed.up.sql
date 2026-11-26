-- The preference sync into ClickHouse is gone: the server logs each reaction
-- and favorite to the activity log as it writes it. Its checkpoints and the
-- revisions it exported by go with it.
DROP TABLE content_preference_cursor;
DROP TABLE content_preference_sync;
DROP INDEX content_reactions_revision_idx;
DROP INDEX content_favorites_revision_idx;
ALTER TABLE content_reactions DROP COLUMN revision;
ALTER TABLE content_favorites DROP COLUMN revision;
DROP SEQUENCE content_preference_revision_seq;

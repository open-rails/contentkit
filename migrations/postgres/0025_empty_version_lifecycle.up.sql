-- Only an automatically empty parent is revived when an edition is restored.
-- Independent moderation and owner-group deletions keep their existing cause.
ALTER TABLE content_items ADD COLUMN deleted_without_versions boolean NOT NULL DEFAULT false;

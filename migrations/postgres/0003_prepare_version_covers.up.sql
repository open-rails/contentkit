-- Prepare both supported baseline histories for version-owned covers.
-- The fresh baseline already has this column; the recorded older baseline
-- does not. Commit preparation before the separate guarded cleanup so a
-- populated installation can run its explicit media-copy step between them.
ALTER TABLE content_versions ADD COLUMN IF NOT EXISTS cover jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'content_versions'::regclass
          AND conname = 'content_versions_cover_check'
    ) THEN
        ALTER TABLE content_versions ADD CONSTRAINT content_versions_cover_check
            CHECK (cover IS NULL OR jsonb_typeof(cover) = 'object');
    END IF;
END;
$$;

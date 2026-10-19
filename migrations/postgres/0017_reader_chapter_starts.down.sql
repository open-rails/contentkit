-- Never guess page indices or lose authored starts during a rollback.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM content_segments WHERE start_page IS NOT NULL) THEN
        RAISE EXCEPTION 'anchored chapters must be explicitly converted before rollback';
    END IF;
END $$;
DROP INDEX content_segments_start_page_idx;
ALTER TABLE content_segments DROP CONSTRAINT content_segments_range_check;
ALTER TABLE content_segments DROP COLUMN start_page;
ALTER TABLE content_segments ALTER COLUMN start_index SET NOT NULL;
ALTER TABLE content_segments ALTER COLUMN end_index SET NOT NULL;
ALTER TABLE content_segments ADD CONSTRAINT content_segments_range_check CHECK (start_index >= 0 AND end_index > start_index);

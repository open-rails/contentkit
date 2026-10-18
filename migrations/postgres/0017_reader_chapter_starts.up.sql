-- Anchored reader chapters store only a stable source page identity. Retain
-- numeric ranges for older records and non-reader consumers.
ALTER TABLE content_segments ADD COLUMN start_page text;
ALTER TABLE content_segments ALTER COLUMN start_index DROP NOT NULL;
ALTER TABLE content_segments ALTER COLUMN end_index DROP NOT NULL;
ALTER TABLE content_segments DROP CONSTRAINT content_segments_range_check;
ALTER TABLE content_segments ADD CONSTRAINT content_segments_range_check CHECK (
    (start_page IS NULL AND start_index IS NOT NULL AND end_index IS NOT NULL AND start_index >= 0 AND end_index > start_index)
    OR (start_page IS NOT NULL AND start_page ~ '^[0-9a-f]{64}$' AND start_index IS NULL AND end_index IS NULL)
);
CREATE UNIQUE INDEX content_segments_start_page_idx
    ON content_segments (tenant_id, version_id, start_page) WHERE start_page IS NOT NULL;

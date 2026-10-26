-- Prepared rankings belong to declared host buckets. Preserve canonical
-- engagement; previous cached scores are rebuilt under the new bucket policy.
ALTER TABLE content_popular_rankings
    ADD COLUMN window_from timestamptz,
    ADD COLUMN window_until timestamptz,
    ADD COLUMN covered_from timestamptz,
    ADD COLUMN covered_until timestamptz,
    ADD COLUMN available_from timestamptz,
    DROP COLUMN preset,
    DROP COLUMN requested_at;

UPDATE content_popular_rankings SET computed_at = NULL, content_ids = '{}';

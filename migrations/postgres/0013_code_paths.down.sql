UPDATE content_codes SET slug = split_part(slug, '/', 2) WHERE slug LIKE '%/%';
UPDATE content_codes SET slugs = (SELECT coalesce(jsonb_object_agg(e.key, CASE WHEN e.value LIKE '%/%' THEN split_part(e.value, '/', 2) ELSE e.value END), '{}'::jsonb)
    FROM jsonb_each_text(slugs) AS e) WHERE slugs::text LIKE '%/%';
ALTER TABLE content_codes DROP CONSTRAINT content_codes_slug_ck;
ALTER TABLE content_codes ADD CONSTRAINT content_codes_slug_ck
    CHECK ((slug ~ '^([a-z0-9]+(-[a-z0-9]+)*)?$'::text) AND (length(slug) <= 80));

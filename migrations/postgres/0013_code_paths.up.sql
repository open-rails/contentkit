-- A content code's slug is its decorative path: an entity slug and a title
-- slug, each a Slugify slug, joined by "/" (contenturl.Path).
ALTER TABLE content_codes DROP CONSTRAINT content_codes_slug_ck;
ALTER TABLE content_codes ADD CONSTRAINT content_codes_slug_ck
    CHECK ((slug ~ '^([a-z0-9]+(-[a-z0-9]+)*(/[a-z0-9]+(-[a-z0-9]+)*)?)?$'::text) AND (length(slug) <= 161));

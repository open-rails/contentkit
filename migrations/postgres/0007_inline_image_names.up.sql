-- parent: 6 sha256:3920c35c3b0bc57053a4f50a79be0d5d703175b5bb31e1ae92d17a7ca08b9e81

-- #101 is a coordinated empty-media cutover, not a legacy URL backfill.
LOCK TABLE content_posts, content_poll_questions, content_poll_options IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM content_posts WHERE coalesce(cover_url, '') <> '')
        OR EXISTS (SELECT 1 FROM content_poll_questions WHERE coalesce(image_url, '') <> '')
        OR EXISTS (SELECT 1 FROM content_poll_options WHERE coalesce(image_url, '') <> '') THEN
        RAISE EXCEPTION 'legacy image URLs exist; coordinate the media cutover before applying inline image names';
    END IF;
END;
$$;

ALTER TABLE content_posts RENAME COLUMN cover_url TO cover_name;
ALTER TABLE content_poll_questions RENAME COLUMN image_url TO image_name;
ALTER TABLE content_poll_options RENAME COLUMN image_url TO image_name;

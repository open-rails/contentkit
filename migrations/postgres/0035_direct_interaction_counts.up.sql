-- Interaction writes already maintain one aggregate in their own transaction.
-- Readers now join it directly; facets no longer store an asynchronous copy.
ALTER TABLE content_facets
    DROP COLUMN likes,
    DROP COLUMN dislikes,
    DROP COLUMN favorites,
    DROP COLUMN rating;

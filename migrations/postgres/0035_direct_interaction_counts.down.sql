ALTER TABLE content_facets
    ADD COLUMN likes integer NOT NULL DEFAULT 0,
    ADD COLUMN dislikes integer NOT NULL DEFAULT 0,
    ADD COLUMN favorites integer NOT NULL DEFAULT 0,
    ADD COLUMN rating double precision NOT NULL DEFAULT 0;

-- Restore the older reader's projection from the still-authoritative totals.
UPDATE content_facets f
SET likes = c.likes, dislikes = c.dislikes, favorites = c.favorites,
    rating = CASE WHEN c.likes + c.dislikes = 0 THEN 0::float8 ELSE
        ((c.likes + 1.9208) / (c.likes + c.dislikes)
          - 1.96 * sqrt(c.likes::float8 * c.dislikes / (c.likes + c.dislikes) + 0.9604) / (c.likes + c.dislikes))
        / (1 + 3.8416 / (c.likes + c.dislikes)) END
FROM content_interaction_counts c
WHERE c.tenant_id = f.tenant_id AND c.content_kind = f.kind
  AND c.content_id = f.content_id AND c.content_version_id IS NULL;

-- Rankings are computed from ClickHouse and cached, not stored: the stored
-- gallery and entity rankings and the favourites index the entity ranking
-- read are gone.
DROP TABLE content_popular_rankings;
DROP TABLE content_entity_popular_rankings;
DROP INDEX content_favorites_popular_idx;

-- A player version's media is playable once its first HLS rendition is out;
-- NULL until media facts are recorded. Listings leave out a player version
-- that is not playable (content_facets.unplayable); its page stays reachable.
ALTER TABLE content_versions ADD COLUMN IF NOT EXISTS playable boolean;
ALTER TABLE content_facets ADD COLUMN IF NOT EXISTS unplayable boolean NOT NULL DEFAULT false;

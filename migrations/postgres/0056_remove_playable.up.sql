-- Publication completion is the only reader-availability gate.
ALTER TABLE content_versions DROP COLUMN playable;
ALTER TABLE content_facets DROP COLUMN unplayable;

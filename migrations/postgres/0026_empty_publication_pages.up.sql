-- Publications written for a version without pages stored their page list as
-- JSON null. Snapshots now always store an array, and the go-live gate
-- compares against it; rewrite the old rows so their versions can go live.
UPDATE content_version_publications SET source = jsonb_set(source, '{pages}', '[]')
WHERE jsonb_typeof(source->'pages') = 'null';

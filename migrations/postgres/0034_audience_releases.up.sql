-- Coordinated forward upgrade: old writers must stop before removing columns.
-- Preserve the exact legacy audience/timestamp in ordinary release entries.
CREATE FUNCTION pg_temp.migrate_content_release(rule jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN rule ? 'free_after' THEN
   (rule - 'free_after') || CASE WHEN rule->'free_after' IS NULL OR rule->'free_after' = 'null'::jsonb THEN '{}'::jsonb
   ELSE jsonb_build_object('releases', coalesce(rule->'releases','[]'::jsonb) || jsonb_build_array(jsonb_build_object('at',rule->'free_after','level','public'))) END
 ELSE rule END
$$;
UPDATE content_items SET access = pg_temp.migrate_content_release(access) WHERE access ? 'free_after';
UPDATE content_versions SET access = pg_temp.migrate_content_release(access) WHERE access ? 'free_after';
UPDATE content_groups SET access = pg_temp.migrate_content_release(access) WHERE access ? 'free_after';
UPDATE content_access_history SET snapshot = pg_temp.migrate_content_release(snapshot) WHERE snapshot ? 'free_after';
DROP FUNCTION pg_temp.migrate_content_release(jsonb);

ALTER TABLE content_facets ADD COLUMN access_releases jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(access_releases)='array'),
 ADD COLUMN next_release_at timestamptz;
UPDATE content_facets f SET access_releases = coalesce(coalesce(v.access,c.access)->'releases','[]'::jsonb)
 FROM content_versions v JOIN content_items c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE f.tenant_id=v.tenant_id AND f.version_id=v.id;
DROP INDEX content_facets_free_idx;
ALTER TABLE content_facets DROP COLUMN access_free_after, DROP COLUMN free_applied;

-- These receipts survive log retention. Reader cursors never own source state.
CREATE TABLE content_release_receipts (
 tenant_id text NOT NULL,
 content_id text NOT NULL,
 version_id text NOT NULL,
 release_key text NOT NULL,
 live_from timestamptz NOT NULL,
 occurred_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,version_id,release_key,live_from),
 FOREIGN KEY(tenant_id,content_id) REFERENCES content_items(tenant_id,id) ON DELETE CASCADE
);

-- Canonical release identity ignores JSON key/tier ordering and timezone spelling.
CREATE FUNCTION content_release_key(release jsonb) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT jsonb_build_object('at',extract(epoch from (release->>'at')::timestamptz),'level',release->>'level',
 'tiers',coalesce((SELECT jsonb_agg(t ORDER BY t) FROM jsonb_array_elements_text(release->'tiers') t),'[]'::jsonb),
 'group_id',release->>'group_id')::text
$$;

UPDATE content_facets f SET next_release_at=(
 SELECT min(greatest(f.live_from,(release->>'at')::timestamptz)) FROM jsonb_array_elements(f.access_releases) release
) WHERE NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL;
CREATE INDEX content_facets_next_release_idx ON content_facets(tenant_id,next_release_at) WHERE next_release_at IS NOT NULL;

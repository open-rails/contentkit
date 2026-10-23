-- Timeless memberships are already refreshed in the writing transaction. Only
-- timed memberships need runtime evaluation; no worker or lookback is involved.
CREATE INDEX content_memberships_timed_content_idx ON content_memberships
 (tenant_id, content_id, version_id)
 WHERE active_from IS NOT NULL OR active_until IS NOT NULL;

CREATE FUNCTION content_current_group_ids(wanted_tenant text, wanted_content text, wanted_version text)
RETURNS text[] LANGUAGE sql STABLE
BEGIN ATOMIC
 RETURN (
 WITH RECURSIVE effective(id) AS (
  SELECT m.group_id FROM content_memberships m
  JOIN content_groups g ON g.tenant_id=m.tenant_id AND g.id=m.group_id AND g.state='active'
  WHERE m.tenant_id=wanted_tenant AND m.content_id=wanted_content
   AND (m.version_id IS NULL OR m.version_id=wanted_version) AND m.state='active'
   AND (m.active_from IS NULL OR m.active_from<=now())
   AND (m.active_until IS NULL OR m.active_until>now())
  UNION
  SELECT p.id FROM effective e
  JOIN content_groups c ON c.tenant_id=wanted_tenant AND c.id=e.id
  JOIN content_groups p ON p.tenant_id=c.tenant_id AND p.id=c.parent_id AND p.state='active'
 ) SELECT coalesce(array_agg(id ORDER BY id), '{}'::text[]) FROM effective
 );
END;

-- Separate branches retain the ordinary facet indexes for timeless content.
-- Do not depend on retired popularity/view counters: operators may remove them.
CREATE VIEW content_current_facets AS
 SELECT f.tenant_id,f.content_id,f.version_id,f.kind,f.live_from,f.deleted,f.languages,
 f.access_level,f.access_tiers,f.access_group_id,f.access_releases,f.next_release_at,
 f.restrictions,f.group_ids,f.traits,f.owner_group_id,f.position,f.published_at,
 f.attributes,f.updated_at,f.unplayable
 FROM content_facets f
 WHERE NOT EXISTS (SELECT 1 FROM content_memberships m WHERE m.tenant_id=f.tenant_id AND m.content_id=f.content_id
  AND (m.version_id IS NULL OR m.version_id=f.version_id) AND (m.active_from IS NOT NULL OR m.active_until IS NOT NULL))
 UNION ALL
 SELECT f.tenant_id,f.content_id,f.version_id,f.kind,f.live_from,f.deleted,f.languages,
 f.access_level,f.access_tiers,f.access_group_id,f.access_releases,f.next_release_at,
 f.restrictions,content_current_group_ids(f.tenant_id,f.content_id,f.version_id),f.traits,f.owner_group_id,f.position,f.published_at,
 f.attributes,f.updated_at,f.unplayable
 FROM content_facets f
 WHERE EXISTS (SELECT 1 FROM content_memberships m WHERE m.tenant_id=f.tenant_id AND m.content_id=f.content_id
  AND (m.version_id IS NULL OR m.version_id=f.version_id) AND (m.active_from IS NOT NULL OR m.active_until IS NOT NULL));

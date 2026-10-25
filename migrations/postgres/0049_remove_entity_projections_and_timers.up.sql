-- Refuse silent conversion of dates into permanent public associations.
-- Hosts with dated records must explicitly remove/convert them before upgrade.
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM content_memberships WHERE active_from IS NOT NULL OR active_until IS NOT NULL) THEN
  RAISE EXCEPTION 'dated content memberships must be explicitly removed or converted before this upgrade';
 END IF;
END;
$$;

DROP FUNCTION content_current_entity_ids(text,text,text);
CREATE OR REPLACE FUNCTION content_keyword_entity_ids(wanted_tenant text, wanted_content text)
RETURNS text[] LANGUAGE sql STABLE
BEGIN ATOMIC
 RETURN (
 WITH RECURSIVE effective(id) AS (
  SELECT m.entity_id FROM content_memberships m
  JOIN content_entities g ON g.tenant_id=m.tenant_id AND g.id=m.entity_id AND g.state='active'
  WHERE m.tenant_id=wanted_tenant AND m.content_id=wanted_content AND m.state='active'
  UNION
  SELECT p.id FROM effective e
  JOIN content_entities c ON c.tenant_id=wanted_tenant AND c.id=e.id
  JOIN content_entities p ON p.tenant_id=c.tenant_id AND p.id=c.parent_id AND p.state='active'
 ) SELECT coalesce(array_agg(id ORDER BY id), '{}'::text[]) FROM effective
 );
END;

DROP INDEX content_memberships_timed_content_idx;
DROP INDEX content_memberships_window_idx;
ALTER TABLE content_memberships
 DROP CONSTRAINT content_memberships_window_check,
 DROP COLUMN active_from,
 DROP COLUMN active_until;

-- Counts and automatic covers derive from live facets on reads.
DROP TABLE content_entity_counts;

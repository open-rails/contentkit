-- Stop old API/media writers before this upgrade. This migration preserves
-- already-effective publication facts; it does not perform the required
-- originals/future/served object conversion. The matching runtime must refuse
-- legacy object placement until that explicit conversion has completed.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM content_items WHERE coalesce((access#>>'{preview,segments}')::integer,0)<>0)
       OR EXISTS (SELECT 1 FROM content_versions WHERE coalesce((access#>>'{preview,segments}')::integer,0)<>0) THEN
        RAISE EXCEPTION 'ContentKit chapter publication: stored free-chapter preview rules require explicit conversion to item access or public preview renditions before retrying.';
    END IF;
END
$$;

ALTER TABLE content_versions ADD COLUMN released boolean NOT NULL DEFAULT false;

-- A gallery may supply a creation default, but each edition owns its policy.
-- Snapshot the effective rule once before removing runtime inheritance.
UPDATE content_versions v SET access=c.access
FROM content_items c WHERE c.tenant_id=v.tenant_id AND c.id=v.content_id AND v.access IS NULL;
ALTER TABLE content_versions ALTER COLUMN access SET NOT NULL;

-- The old model made dates effective without a worker. Preserve that exact
-- pre-upgrade state once; new writes can set released only after storage work.
UPDATE content_versions v
SET released=true,
    published_at=coalesce(v.published_at,greatest(v.publish_at,c.published_at,c.publish_at))
FROM content_items c
WHERE c.tenant_id=v.tenant_id AND c.id=v.content_id
  AND (c.state='published' OR (c.state='deleted' AND c.published_at IS NOT NULL))
  AND (v.state='published' OR (v.state='deleted' AND v.published_at IS NOT NULL))
  AND coalesce(c.published_at,c.publish_at)<=statement_timestamp()
  AND coalesce(v.published_at,v.publish_at,c.published_at,c.publish_at)<=statement_timestamp();

UPDATE content_items c SET published_at=coalesce(c.published_at,c.publish_at)
WHERE c.published_at IS NULL AND EXISTS (
    SELECT 1 FROM content_versions v
    WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.released
);

ALTER TABLE content_segments
    ADD COLUMN id text NOT NULL DEFAULT gen_random_uuid()::text,
    ADD COLUMN released_at timestamptz,
    ADD COLUMN release_id uuid;
CREATE UNIQUE INDEX content_segments_id_idx ON content_segments(tenant_id,version_id,id);

-- Resolve each old hash or numeric marker through its exact retained reader
-- snapshot. An absent or ambiguous boundary is a refusal, not a guessed page.
CREATE TEMP TABLE ck250_chapter_boundaries ON COMMIT DROP AS
SELECT s.tenant_id,s.version_id,s.position,s.start_page,s.start_index,s.end_index,
       CASE WHEN jsonb_typeof(p.source->'pages')='array' THEN jsonb_array_length(p.source->'pages') ELSE 0 END AS page_count,
       marker.matches,marker.start_index AS resolved_start,marker.path AS resolved_path
FROM content_segments s
LEFT JOIN content_version_publications p
  ON p.tenant_id=s.tenant_id AND p.version_id=s.version_id AND p.kind='pages' AND p.state='current'
LEFT JOIN LATERAL (
    SELECT count(*) AS matches,min(n-1)::integer AS start_index,min(page->>'path') AS path
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(p.source->'pages')='array' THEN p.source->'pages' ELSE '[]'::jsonb END) WITH ORDINALITY AS pages(page,n)
    WHERE (s.start_page IS NULL AND n-1=s.start_index)
       OR (s.start_page IS NOT NULL AND substring(page->>'source' FROM 'sha256-([0-9a-f]{64})')=s.start_page)
) marker ON true;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ck250_chapter_boundaries WHERE matches<>1 OR coalesce(resolved_path,'')='') THEN
        RAISE EXCEPTION 'ContentKit chapter publication: a legacy chapter has no unique file marker in its retained reader snapshot. Repair or explicitly resolve the chapter with the previous version before retrying.';
    END IF;
    IF EXISTS (
        SELECT 1 FROM (
            SELECT *,lag(resolved_start) OVER (PARTITION BY tenant_id,version_id ORDER BY position) AS previous_start,
                   lead(resolved_start,1,page_count) OVER (PARTITION BY tenant_id,version_id ORDER BY position) AS next_start
            FROM ck250_chapter_boundaries
        ) chapters
        WHERE resolved_start<=previous_start OR (start_page IS NULL AND end_index<>next_start)
    ) THEN
        RAISE EXCEPTION 'ContentKit chapter publication: legacy chapter ranges are reordered, overlapping or have gaps. Resolve them explicitly before converting to manifest start markers.';
    END IF;
END
$$;

ALTER TABLE content_segments DROP CONSTRAINT content_segments_range_check;
UPDATE content_segments s
SET start_page=b.resolved_path,start_index=NULL,end_index=NULL
FROM ck250_chapter_boundaries b
WHERE b.tenant_id=s.tenant_id AND b.version_id=s.version_id AND b.position=s.position;

-- Keep an explicit numeric-range compatibility input for import conversion;
-- authored markers now name virtual manifest files, not content hashes.
ALTER TABLE content_segments ADD CONSTRAINT content_segments_range_check CHECK (
    (start_page IS NULL AND start_index IS NOT NULL AND end_index IS NOT NULL AND start_index>=0 AND end_index>start_index)
    OR (start_page IS NOT NULL AND octet_length(start_page) BETWEEN 1 AND 1024
        AND start_page !~ '(^/|(^|/)\.\.?(/|$)|[[:cntrl:]\\])'
        AND start_page NOT LIKE '%//%' AND start_page NOT LIKE '%/'
        AND start_index IS NULL AND end_index IS NULL)
);

WITH prior AS (
    SELECT tenant_id,version_id,id,
           bool_and(release_at IS NULL OR release_at<=statement_timestamp()) OVER
               (PARTITION BY tenant_id,version_id ORDER BY position ROWS UNBOUNDED PRECEDING) AS effective,
           max(release_at) OVER
               (PARTITION BY tenant_id,version_id ORDER BY position ROWS UNBOUNDED PRECEDING) AS chapter_at
    FROM content_segments
)
UPDATE content_segments s
SET released_at=greatest(v.published_at,c.published_at,p.chapter_at)
FROM prior p,content_versions v,content_items c
WHERE p.tenant_id=s.tenant_id AND p.version_id=s.version_id AND p.id=s.id AND p.effective
  AND v.tenant_id=s.tenant_id AND v.id=s.version_id AND v.released
  AND c.tenant_id=v.tenant_id AND c.id=v.content_id;

-- Existing search/current-facet views consume this projection. Remove future
-- clock-only live_from values immediately rather than waiting for a refresh.
UPDATE content_facets f
SET live_from=CASE
      WHEN v.released AND c.published_at IS NOT NULL AND v.published_at IS NOT NULL
       AND (c.state='published' OR (c.state='deleted' AND c.published_at IS NOT NULL))
       AND (v.state='published' OR (v.state='deleted' AND v.published_at IS NOT NULL))
      THEN greatest(c.published_at,v.published_at)
    END,
    published_at=c.published_at
FROM content_versions v,content_items c
WHERE v.tenant_id=f.tenant_id AND v.id=f.version_id
  AND c.tenant_id=v.tenant_id AND c.id=v.content_id;

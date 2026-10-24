-- Content items are no longer moderated (comments still are). A live item
-- whose edit is held or was rejected gets its approved text back, with its
-- decorative paths; search documents were already built from that text. A
-- never-published item a reviewer rejected returns to draft; one still held
-- publishes as scheduled.
UPDATE content_codes cd SET
    slugs = coalesce((SELECT jsonb_object_agg(s.language, s.approved) FROM (
        SELECT p.key AS language, CASE
            WHEN p.value = coalesce(t.slug, '') THEN c.published_snapshot -> p.key ->> 'slug'
            WHEN right(p.value, length(coalesce(t.slug, '')) + 1) = '/' || coalesce(t.slug, '')
                THEN left(p.value, length(p.value) - length(coalesce(t.slug, ''))) || (c.published_snapshot -> p.key ->> 'slug')
            ELSE p.value END AS approved
        FROM jsonb_each_text(cd.slugs) p
        LEFT JOIN content_text t ON t.tenant_id = c.tenant_id AND t.content_id = c.id AND t.language = p.key
        WHERE c.published_snapshot ? p.key) s), '{}'),
    slug = coalesce((SELECT CASE
            WHEN cd.slug = coalesce(t.slug, '') THEN c.published_snapshot -> t.language ->> 'slug'
            ELSE left(cd.slug, length(cd.slug) - length(t.slug)) || (c.published_snapshot -> t.language ->> 'slug') END
        FROM content_text t
        WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id AND c.published_snapshot ? t.language
          AND (cd.slug = t.slug OR right(cd.slug, length(t.slug) + 1) = '/' || t.slug)
        ORDER BY t.language LIMIT 1), cd.slug),
    updated_at = now()
FROM content_items c
WHERE c.published_snapshot IS NOT NULL AND cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id;

DELETE FROM content_text t USING content_items c
WHERE c.published_snapshot IS NOT NULL AND t.tenant_id = c.tenant_id AND t.content_id = c.id AND NOT c.published_snapshot ? t.language;

INSERT INTO content_text (tenant_id, content_id, language, title, slug, description, body, sort_key, aliases, updated_at)
SELECT c.tenant_id, c.id, s.key, coalesce(s.value ->> 'title', ''), coalesce(s.value ->> 'slug', ''), coalesce(s.value ->> 'description', ''),
    coalesce(s.value ->> 'body', ''), coalesce(s.value ->> 'sort_key', ''),
    ARRAY(SELECT jsonb_array_elements_text(coalesce(s.value -> 'aliases', '[]'))), now()
FROM content_items c CROSS JOIN LATERAL jsonb_each(c.published_snapshot) s
WHERE c.published_snapshot IS NOT NULL
ON CONFLICT (tenant_id, content_id, language) DO UPDATE SET title = EXCLUDED.title, slug = EXCLUDED.slug, description = EXCLUDED.description,
    body = EXCLUDED.body, sort_key = EXCLUDED.sort_key, aliases = EXCLUDED.aliases, updated_at = EXCLUDED.updated_at;

UPDATE content_items SET state = 'draft', publish_at = NULL, revision = revision + 1, updated_at = now()
WHERE moderation = 'rejected' AND published_snapshot IS NULL AND state = 'published';

ALTER TABLE content_items DROP COLUMN published_snapshot, DROP COLUMN moderation, DROP COLUMN moderation_reason,
    DROP COLUMN moderation_verdict, DROP COLUMN moderated_by, DROP COLUMN moderated_at, DROP COLUMN moderation_revision;
